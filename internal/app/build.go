package app

import (
	"context"
	"crypto/sha1" //nolint:gosec // record ids, not a security use
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/types"
	"github.com/jackc/pgx/v5"
)

const uuidPattern = `[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`
const maxSafeInt = 9007199254740991

type FieldPlan struct {
	PGName     string   `json:"pg"`
	PGType     string   `json:"pgType"`
	Name       string   `json:"pb"`
	Kind       string   `json:"kind"`
	OnlyInt    bool     `json:"onlyInt,omitempty"`
	Required   bool     `json:"required,omitempty"`
	Values     []string `json:"values,omitempty"`
	MaxSelect  int      `json:"maxSelect,omitempty"`
	RelTable   string   `json:"relation,omitempty"` // target collection
	RelPG      string   `json:"-"`
	Cascade    bool     `json:"cascade,omitempty"`
	Bucket     string   `json:"bucket,omitempty"`
	Min        int      `json:"min,omitempty"`
	Max        int      `json:"max,omitempty"`
	DefaultLit any      `json:"default,omitempty"`
	DefaultMe  bool     `json:"defaultAuthUid,omitempty"` // DEFAULT auth.uid(): filled with the signed-in user by a hook
	OnUpdate   bool     `json:"onUpdate,omitempty"`       // autodate also set on update (updated_at trigger)
	NoOnCreate bool     `json:"-"`
	MaxSize    int64    `json:"maxSize,omitempty"`
	Note       string   `json:"note,omitempty"`
}

type CollPlan struct {
	Table  *Table                 `json:"-"`
	PGName string                 `json:"pg"`
	Name   string                 `json:"pb"`
	Users  bool                   `json:"mergedIntoUsers,omitempty"`
	IdMode string                 `json:"idMode"`
	Fields []*FieldPlan           `json:"fields"`
	Rules  map[string]*RuleResult `json:"rules"`
	Coll   *core.Collection       `json:"-"`
	Rows   int64                  `json:"sourceRows"`
	Loaded int64                  `json:"imported"`
}

type FileJob struct {
	CollID, RecID, Bucket, Object, Filename string
	Data                                    []byte // inline content (bytea columns)
}

type Report struct {
	Plans      []*CollPlan       `json:"collections"`
	Users      map[string]any    `json:"users"`
	Externals  map[string]int    `json:"externalAuths"`
	Files      map[string]any    `json:"files"`
	Views      []map[string]any  `json:"views"`
	Storage    []*CollPlan       `json:"storageCollections"`
	Timings    map[string]string `json:"timings"`
	ResetUsers []string          `json:"usersRequiringPasswordReset"`
	NoPassword []string          `json:"usersWithoutPassword"` // OAuth / OTP only: nothing to reset
	Hooks      []string          `json:"generatedHooks"`
	Warnings   map[string]int    `json:"warnings"`
}

type Builder struct {
	ctx        context.Context
	db         *pgx.Conn
	app        core.App
	a          *Analysis
	storageURL string
	serviceKey string
	plans      []*CollPlan
	byPG       map[string]*CollPlan
	users      *CollPlan
	jobs       []FileJob
	linked     map[string]bool
	rep        *Report
	provider   map[string]string
	warnings   map[string]int
	skipFiles  bool
	http       *http.Client
	mu         sync.Mutex
	curRow     map[string]json.RawMessage
}

// usersReserved are the fields of the users collection a merged profile column must never overwrite: PocketBase's
// own auth fields and the ones importUsers fills from auth.users. Compared in lower case.
var usersReserved = map[string]bool{"id": true, "email": true, "emailvisibility": true, "verified": true, "password": true,
	"tokenkey": true, "created": true, "updated": true, "phone": true, "user_metadata": true, "app_metadata": true,
	"banned_until": true, "last_sign_in_at": true, "password_reset_required": true}

// profileTableNames are the names that mark the profile table when several tables are keyed by the user id.
var profileTableNames = []string{"profiles", "profile", "users", "user_profiles"}

// keyedByUser reports whether a table has one row per user: its primary key is a foreign key to auth.users.
func keyedByUser(t *Table) bool {
	if len(t.PK) != 1 {
		return false
	}
	fk := t.FKFor(t.PK[0])
	return fk != nil && fk.RefTable == "auth.users"
}

// pickProfileTable chooses the one table that is merged into the users collection. Only one can be: a second
// one would bring its own columns and rules. A single candidate is merged whatever its name; among several,
// the one with a profile-like name; with no such name none is merged. Tables that are not merged stay
// collections of their own, with the user id as record id, so nothing is dropped.
func pickProfileTable(tables []*Table) *Table {
	var cands []*Table
	for _, t := range tables {
		if keyedByUser(t) {
			cands = append(cands, t)
		}
	}
	if len(cands) == 1 {
		return cands[0]
	}
	for _, name := range profileTableNames {
		for _, t := range cands {
			if t.Name == name {
				return t
			}
		}
	}
	return nil
}

// mergedFieldName names a profile column inside the users collection. full_name and avatar_url take the built-in
// name and avatar fields unless the table has columns with those names already; a column that would overwrite a
// field filled from auth.users (email, verified, password, phone...) gets a profile_ prefix instead.
func mergedFieldName(t *Table, col, kind string) (name, note string) {
	switch {
	case col == "full_name" && t.Col("name") == nil:
		return "name", ""
	case col == "avatar_url" && kind == "file" && t.Col("avatar") == nil:
		return "avatar", ""
	case usersReserved[strings.ToLower(col)] || (col == "avatar" && kind != "file"):
		name = "profile_" + col
		for t.Col(name) != nil {
			name += "_"
		}
		return name, fmt.Sprintf("renamed: users.%s is filled from auth.users", col)
	}
	return col, ""
}

func (b *Builder) plan() {
	b.byPG = map[string]*CollPlan{}
	profile := pickProfileTable(b.a.Tables)
	for _, t := range b.a.Tables {
		cp := &CollPlan{Table: t, PGName: t.Name, Name: t.Name, Rows: t.Rows}
		if t == profile {
			cp.Users, cp.Name = true, "users"
			b.users = cp
		}
		var pkc *Column
		if len(t.PK) == 1 {
			pkc = t.Col(t.PK[0])
		}
		switch {
		case pkc != nil && pkc.TypName == "uuid":
			cp.IdMode = "uuid"
		case pkc != nil && (pkc.TypName == "int2" || pkc.TypName == "int4" || pkc.TypName == "int8"):
			cp.IdMode = "int"
		case pkc != nil && textTypes[pkc.TypName] && pkc.BadPK == 0:
			cp.IdMode = "text"
		default:
			cp.IdMode = "hash" // composite PK, no PK, or text keys PocketBase cannot use as ids
		}
		b.plans = append(b.plans, cp)
		b.byPG[t.Name] = cp
	}
	target := func(ref string) (string, string) {
		if ref == "auth.users" {
			return "users", "users"
		}
		t := strings.TrimPrefix(ref, "public.")
		if p := b.byPG[t]; p != nil && p.Users {
			return "users", t
		}
		return t, t
	}
	reDefStr := regexp.MustCompile(`^'(.*)'::[\w.]+$`)
	for _, cp := range b.plans {
		t := cp.Table
		for _, c := range t.Columns {
			fp := &FieldPlan{PGName: c.Name, PGType: c.Type, Name: c.Name}
			cp.Fields = append(cp.Fields, fp)
			isPK := len(t.PK) == 1 && t.PK[0] == c.Name
			if isPK && cp.IdMode != "hash" {
				fp.Kind, fp.Name = "id", "id"
				switch {
				case !cp.Users && keyedByUser(t):
					fp.Note = "references auth.users -> record id = the user's id (one record per user, kept as its own collection); deleting a user does not delete it"
				case strings.Contains(c.Default, "gen_random_uuid") || strings.Contains(c.Default, "uuid_generate"):
					fp.Note = "DEFAULT gen_random_uuid() -> id autogenerate pattern (UUIDv4 shape)"
				case cp.IdMode == "int":
					fp.Note = "integer PK -> string id (\"1\"); new records get random 15-char ids (no sequence)"
				case cp.IdMode == "text":
					fp.Note = "text PK kept as id (all values valid PocketBase ids)"
				}
				continue
			}
			if fk := t.FKFor(c.Name); fk != nil && len(fk.RefCols) == 1 {
				relColl, relPG := target(fk.RefTable)
				ok := fk.RefTable == "auth.users" && fk.RefCols[0] == "id"
				if rt := b.a.Table(relPG); rt != nil && len(rt.PK) == 1 && rt.PK[0] == fk.RefCols[0] {
					ok = true
				}
				if ok {
					fp.Kind, fp.MaxSelect, fp.Required, fp.Cascade = "relation", 1, c.NotNull, fk.OnDelete == "c"
					fp.RelTable, fp.RelPG = relColl, relPG
					if fk.RefCols[0] != "id" {
						fp.Note = fmt.Sprintf("FK -> %s.%s (PK not named id)", relPG, fk.RefCols[0])
					}
					if p := b.byPG[relPG]; p != nil && p.IdMode == "hash" {
						fp.Note = strings.TrimPrefix(fp.Note+"; ", "; ") + "target ids are hashed keys -> FK values remapped"
					}
					goto defaults
				}
				fp.Note = "FK to a non-PK column -> kept as plain value"
			}
			if c.LinkBucket != "" {
				fp.Kind, fp.Bucket = "file", c.LinkBucket
				fp.Note = fmt.Sprintf("values pointed to storage bucket '%s' (%.0f%%) -> file field, objects copied", c.LinkBucket, c.LinkRatio*100)
				goto defaults
			}
			mapColumn(c, fp)
			if isPK {
				fp.Note = strings.TrimPrefix(fp.Note+"; ", "; ") + "original key kept here (+unique index); id = sha1(key)"
			}
		defaults:
			if reAuthUIDDefault.MatchString(c.Default) && fp.Kind == "relation" {
				fp.DefaultMe = true
			}
			if m := reDefStr.FindStringSubmatch(c.Default); m != nil && m[1] != "" && m[1] != "{}" {
				fp.DefaultLit = m[1]
			} else if c.Default == "true" {
				fp.DefaultLit = true
			}
			if cp.Users {
				var renamed string
				fp.Name, renamed = mergedFieldName(t, c.Name, fp.Kind)
				if renamed != "" {
					fp.Note = strings.TrimPrefix(fp.Note+"; "+renamed, "; ")
				}
				if c.Name == "created_at" {
					fp.Kind, fp.Note = "skip", "duplicate of auth.users.created_at (users.created)"
				}
			}
		}
		for _, ck := range t.Checks {
			if m := reCharLen.FindStringSubmatch(ck); m != nil && m[1] == m[3] {
				for _, fp := range cp.Fields {
					if fp.PGName == m[1] {
						fp.Min, _ = strconv.Atoi(m[2])
						fp.Max, _ = strconv.Atoi(m[4])
						fp.Note = strings.TrimSpace(fp.Note + " CHECK char_length -> min/max")
					}
				}
			}
		}
		if cp.IdMode == "hash" {
			what := "composite PK (" + strings.Join(t.PK, ",") + ")"
			if len(t.PK) == 1 {
				what = "text PK with values PocketBase rejects as ids (/ . space, reserved names...)"
			} else if len(t.PK) == 0 {
				what = "no PK"
			}
			cp.Fields = append([]*FieldPlan{{Name: "id", Kind: "id", Note: what + " -> id = sha1(key)[:15]"}}, cp.Fields...)
		}
	}
}

func (cp *CollPlan) field(pg string) *FieldPlan {
	for _, f := range cp.Fields {
		if f.PGName == pg {
			return f
		}
	}
	return nil
}

func configureID(c *core.Collection, mode string) {
	f := c.Fields.GetByName("id").(*core.TextField)
	switch mode {
	case "uuid":
		f.Pattern, f.Min, f.Max, f.AutogeneratePattern = `^[a-z0-9-]+$`, 1, 36, uuidPattern
	case "int":
		f.Pattern, f.Min, f.Max = `^[a-z0-9]+$`, 1, 20
	case "text":
		f.Pattern, f.Min, f.Max = "^[^\\s./\\\\|\"'`<>:?*%$]+$", 1, 100
	}
}

func (b *Builder) bucket(id string) *Bucket {
	for i := range b.a.Buckets {
		if b.a.Buckets[i].ID == id {
			return &b.a.Buckets[i]
		}
	}
	return &Bucket{ID: id}
}

func roundUpMB(n int64) int64 {
	mb := int64(1 << 20)
	if n < 5*mb {
		return 5 * mb
	}
	return ((n / mb) + 1) * mb
}

func (b *Builder) makeField(fp *FieldPlan, colls map[string]*core.Collection) core.Field {
	switch fp.Kind {
	case "text":
		return &core.TextField{Name: fp.Name, Required: fp.Required, Min: fp.Min, Max: fp.Max}
	case "number":
		return &core.NumberField{Name: fp.Name, OnlyInt: fp.OnlyInt}
	case "bool":
		return &core.BoolField{Name: fp.Name}
	case "date":
		return &core.DateField{Name: fp.Name, Required: fp.Required}
	case "autodate":
		return &core.AutodateField{Name: fp.Name, OnCreate: !fp.NoOnCreate, OnUpdate: fp.OnUpdate}
	case "json":
		return &core.JSONField{Name: fp.Name, MaxSize: fp.MaxSize}
	case "geopoint":
		return &core.GeoPointField{Name: fp.Name}
	case "bytes":
		return &core.FileField{Name: fp.Name, MaxSelect: 1, MaxSize: roundUpMB(fp.MaxSize), Protected: true}
	case "select":
		return &core.SelectField{Name: fp.Name, Values: fp.Values, MaxSelect: fp.MaxSelect, Required: fp.Required}
	case "relation":
		return &core.RelationField{Name: fp.Name, CollectionId: colls[fp.RelTable].Id, MaxSelect: 1, CascadeDelete: fp.Cascade, Required: fp.Required}
	case "file":
		bk := b.bucket(fp.Bucket)
		return &core.FileField{Name: fp.Name, MaxSelect: 1, MaxSize: roundUpMB(bk.MaxSize), Protected: !bk.Public, Thumbs: []string{"100x100"}}
	}
	return nil
}

func (b *Builder) createCollections() error {
	colls := map[string]*core.Collection{}
	users, err := b.app.FindCollectionByNameOrId("users")
	if err != nil {
		return err
	}
	colls["users"] = users
	// pass 1: collections + non-relation fields
	for _, cp := range b.plans {
		var c *core.Collection
		if cp.Users {
			c = users
		} else {
			c = core.NewBaseCollection(cp.Name)
		}
		configureID(c, cp.IdMode)
		for _, fp := range cp.Fields {
			if fp.Kind == "relation" || fp.Kind == "id" || fp.Kind == "skip" {
				continue
			}
			if cp.Users && fp.Name == "avatar" {
				if ff, ok := c.Fields.GetByName("avatar").(*core.FileField); ok {
					bk := b.bucket(fp.Bucket)
					ff.MaxSize, ff.Protected, ff.MimeTypes = roundUpMB(bk.MaxSize), !bk.Public, nil
					continue
				}
			}
			if cp.Users && fp.Name == "name" {
				continue // reuse default users.name text field
			}
			c.Fields.Add(b.makeField(fp, colls))
		}
		if cp.Users {
			c.Fields.Add(&core.TextField{Name: "phone"}, &core.JSONField{Name: "user_metadata"}, &core.JSONField{Name: "app_metadata"},
				&core.DateField{Name: "banned_until"}, &core.DateField{Name: "last_sign_in_at"}, &core.BoolField{Name: "password_reset_required"})
			c.AuthRule = types.Pointer(`banned_until = "" || banned_until < @now`)
		}
		if err := b.app.Save(c); err != nil {
			return fmt.Errorf("create %s: %w", cp.Name, err)
		}
		cp.Coll, colls[cp.Name] = c, c
	}
	if b.users == nil { // no profiles table: still extend users
		b.users = &CollPlan{Name: "users", IdMode: "uuid", Users: true, Coll: users}
		configureID(users, "uuid")
		users.Fields.Add(&core.TextField{Name: "phone"}, &core.JSONField{Name: "user_metadata"}, &core.JSONField{Name: "app_metadata"},
			&core.DateField{Name: "banned_until"}, &core.DateField{Name: "last_sign_in_at"}, &core.BoolField{Name: "password_reset_required"})
		users.AuthRule = types.Pointer(`banned_until = "" || banned_until < @now`)
		if err := b.app.Save(users); err != nil {
			return err
		}
	}
	// pass 2: relations (handles cycles / any order)
	for _, cp := range b.plans {
		changed := false
		for _, fp := range cp.Fields {
			if fp.Kind == "relation" {
				cp.Coll.Fields.Add(b.makeField(fp, colls))
				changed = true
			}
		}
		if changed {
			if err := b.app.Save(cp.Coll); err != nil {
				return fmt.Errorf("relations %s: %w", cp.Name, err)
			}
		}
	}
	return nil
}

// ---------- data ----------

func parseTime(s string) (types.DateTime, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return types.ParseDateTime(t.UTC())
		}
	}
	return types.DateTime{}, fmt.Errorf("bad time %q", s)
}

var reStorageURL = regexp.MustCompile(`^.*/storage/v1/object/(public|authenticated|sign)/[^/]+/`)

func isNull(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

func jsonScalarString(raw json.RawMessage) string {
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s) // row_to_json output: always a valid JSON string here
		return s
	}
	return string(raw) // numbers keep their exact digits
}

var (
	sentinelMax, _ = types.ParseDateTime("9999-12-31 23:59:59.999Z")
	sentinelMin, _ = types.ParseDateTime("0001-01-01 00:00:00.001Z") // exact zero time would be stored as ""
)

func (b *Builder) warn(key string) {
	b.mu.Lock()
	b.warnings[key]++
	b.mu.Unlock()
}

func (b *Builder) setValue(rec *core.Record, fp *FieldPlan, raw json.RawMessage) error {
	switch fp.Kind {
	case "skip", "id":
		return nil
	case "text":
		if isNull(raw) {
			rec.Set(fp.Name, "")
		} else {
			rec.Set(fp.Name, jsonScalarString(raw)) // objects/arrays stay as JSON text
		}
	case "number":
		if isNull(raw) {
			return nil
		}
		if raw[0] == '"' { // "NaN", "Infinity", "-Infinity"
			b.warn(rec.Collection().Name + "." + fp.Name + ": non-finite -> 0")
			rec.Set(fp.Name, 0)
			return nil
		}
		f, err := strconv.ParseFloat(string(raw), 64)
		if err != nil {
			return err
		}
		rec.Set(fp.Name, f)
	case "bool":
		var v bool
		if !isNull(raw) {
			if err := json.Unmarshal(raw, &v); err != nil {
				return fmt.Errorf("%s: %w", fp.Name, err)
			}
		}
		rec.Set(fp.Name, v)
	case "date", "autodate":
		if isNull(raw) {
			return nil
		}
		s := jsonScalarString(raw)
		switch s {
		case "infinity":
			rec.SetRaw(fp.Name, sentinelMax)
			return nil
		case "-infinity":
			rec.SetRaw(fp.Name, sentinelMin)
			return nil
		}
		dt, err := parseTime(s)
		if err != nil {
			return err
		}
		if dt.Time().IsZero() { // 0001-01-01T00:00:00Z is PocketBase's "empty" date
			dt = sentinelMin
			b.warn(rec.Collection().Name + "." + fp.Name + ": 0001-01-01T00:00:00Z -> +1ms (PocketBase treats it as empty)")
		}
		rec.SetRaw(fp.Name, dt)
	case "json":
		if isNull(raw) {
			rec.Set(fp.Name, nil)
			return nil
		}
		rec.Set(fp.Name, types.JSONRaw(raw))
		jf, _ := rec.Collection().Fields.GetByName(fp.Name).(*core.JSONField)
		if jf == nil || jf.ValidateValue(b.ctx, b.app, rec) == nil {
			return nil
		}
		// PocketBase rejects it (duplicate keys / numbers beyond float64): the record could never be re-saved
		if alt, ok := b.curRow["__jb_"+fp.PGName]; ok && !isNull(alt) {
			rec.Set(fp.Name, types.JSONRaw(alt))
			if jf.ValidateValue(b.ctx, b.app, rec) == nil {
				b.warn(rec.Collection().Name + "." + fp.Name + ": invalid for PocketBase -> jsonb-normalized (duplicate keys collapsed)")
				return nil
			}
		}
		quoted, _ := json.Marshal(string(raw))
		rec.Set(fp.Name, types.JSONRaw(quoted))
		b.warn(rec.Collection().Name + "." + fp.Name + ": invalid for PocketBase (number beyond float64) -> stored as JSON string")
	case "geopoint":
		if isNull(raw) {
			return nil
		}
		var gp types.GeoPoint
		if err := json.Unmarshal(raw, &gp); err != nil {
			return err
		}
		rec.Set(fp.Name, gp)
	case "select":
		if isNull(raw) {
			return nil
		}
		if fp.MaxSelect > 1 {
			var arr []*string
			if err := json.Unmarshal(raw, &arr); err != nil {
				return fmt.Errorf("%s: %w", fp.Name, err)
			}
			var out []string
			for _, v := range arr {
				if v != nil {
					out = append(out, *v)
				} else {
					b.warn(rec.Collection().Name + "." + fp.Name + ": NULL array element dropped")
				}
			}
			rec.Set(fp.Name, out)
		} else {
			rec.Set(fp.Name, jsonScalarString(raw))
		}
	case "relation":
		if isNull(raw) {
			return nil
		}
		rec.Set(fp.Name, b.targetID(fp.RelPG, raw))
	case "bytes":
		if isNull(raw) {
			return nil
		}
		data, err := base64.StdEncoding.DecodeString(jsonScalarString(raw))
		if err != nil {
			return err
		}
		name := fp.Name + ".bin"
		rec.SetRaw(fp.Name, name)
		if data == nil {
			data = []byte{}
		}
		b.jobs = append(b.jobs, FileJob{CollID: rec.Collection().Id, RecID: rec.Id, Filename: name, Data: data})
	case "file":
		if isNull(raw) {
			return nil
		}
		v := jsonScalarString(raw)
		if v == "" {
			return nil
		}
		obj := reStorageURL.ReplaceAllString(v, "")
		name := path.Base(obj)
		rec.SetRaw(fp.Name, name)
		b.linked[fp.Bucket+"/"+obj] = true
		b.jobs = append(b.jobs, FileJob{CollID: rec.Collection().Id, RecID: rec.Id, Bucket: fp.Bucket, Object: obj, Filename: name})
	}
	return nil
}

func hashID(parts ...json.RawMessage) string {
	h := sha1.New() //nolint:gosec // record ids for non-uuid keys, not a security use
	for _, p := range parts {
		h.Write(p)
		h.Write([]byte{0x1f})
	}
	return hex.EncodeToString(h.Sum(nil))[:15]
}

func recordID(cp *CollPlan, row map[string]json.RawMessage) string {
	t := cp.Table
	if cp.IdMode == "hash" {
		cols := t.PK
		if len(cols) == 0 {
			for _, c := range t.Columns {
				cols = append(cols, c.Name)
			}
		}
		parts := make([]json.RawMessage, len(cols))
		for i, c := range cols {
			parts[i] = row[c]
		}
		return hashID(parts...)
	}
	return jsonScalarString(row[t.PK[0]])
}

// targetID converts a FK value into the id the referenced record received.
func (b *Builder) targetID(relPG string, raw json.RawMessage) string {
	if p := b.byPG[relPG]; p != nil && p.IdMode == "hash" {
		return hashID(raw)
	}
	return jsonScalarString(raw)
}

func selectSQL(cp *CollPlan) string {
	var cols []string
	for _, c := range cp.Table.Columns {
		cols = append(cols, colExpr(c)+" as "+qi(c.Name))
		if c.TypName == "json" {
			cols = append(cols, qi(c.Name)+"::jsonb as "+qi("__jb_"+c.Name))
		}
	}
	return fmt.Sprintf("select row_to_json(x)::text from (select %s from public.%s) x", strings.Join(cols, ", "), qi(cp.PGName))
}

func (b *Builder) stream(query string, fn func(row map[string]json.RawMessage) error) error {
	rows, err := b.db.Query(b.ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		row := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(s), &row); err != nil {
			return err
		}
		if err := fn(row); err != nil {
			return err
		}
	}
	return rows.Err()
}

// batched writer: SaveNoValidate inside transactions of N records
type batcher struct {
	app  core.App
	buf  []*core.Record
	size int
	n    int64
}

func (bt *batcher) add(r *core.Record) error {
	bt.buf = append(bt.buf, r)
	if len(bt.buf) >= bt.size {
		return bt.flush()
	}
	return nil
}

func (bt *batcher) flush() error {
	if len(bt.buf) == 0 {
		return nil
	}
	err := bt.app.RunInTransaction(func(tx core.App) error {
		for _, r := range bt.buf {
			if err := tx.SaveNoValidate(r); err != nil {
				return fmt.Errorf("record %s: %w", r.Id, err)
			}
		}
		return nil
	})
	bt.n += int64(len(bt.buf))
	bt.buf = bt.buf[:0]
	return err
}

func (b *Builder) importTables() error {
	for _, cp := range b.plans {
		if cp.Users {
			continue // merged table imported together with auth.users
		}
		bt := &batcher{app: b.app, size: 2000}
		err := b.stream(selectSQL(cp), func(row map[string]json.RawMessage) error {
			rec := core.NewRecord(cp.Coll)
			b.curRow = row
			rec.Set("id", recordID(cp, row))
			for _, fp := range cp.Fields {
				if fp.PGName == "" {
					continue
				}
				if err := b.setValue(rec, fp, row[fp.PGName]); err != nil {
					return fmt.Errorf("%s.%s: %w", cp.PGName, fp.PGName, err)
				}
			}
			return bt.add(rec)
		})
		if err != nil {
			return err
		}
		if err := bt.flush(); err != nil {
			return err
		}
		cp.Loaded = bt.n
		fmt.Printf("  %-18s %9s rows\n", cp.Name, fmtInt(bt.n))
	}
	return nil
}

func (b *Builder) importUsers() error {
	var sel, join string
	var merged []*FieldPlan
	if b.users != nil && b.users.Table != nil {
		join = " left join public." + qi(b.users.PGName) + " p on p." + qi(b.users.Table.PK[0]) + " = u.id"
		for _, fp := range b.users.Fields {
			if fp.Kind == "id" || fp.Kind == "skip" {
				continue
			}
			if c := b.users.Table.Col(fp.PGName); c != nil {
				sel += fmt.Sprintf(", %s as %s", strings.ReplaceAll(colExpr(c), qi(c.Name), "p."+qi(c.Name)), qi("p__"+fp.PGName))
			}
			merged = append(merged, fp)
		}
	}
	q := `select row_to_json(x)::text from (select u.id, u.email, u.phone, u.encrypted_password, u.email_confirmed_at, u.phone_confirmed_at,
		u.raw_user_meta_data, u.raw_app_meta_data, u.created_at, u.updated_at, u.last_sign_in_at, u.banned_until, u.is_anonymous` + sel +
		` from auth.users u` + join + ` where u.deleted_at is null order by u.created_at) x`
	coll := b.users.Coll
	bt := &batcher{app: b.app, size: 500}
	stats := map[string]any{}
	var imported, skippedAnon, bcryptN, reset, noPw int
	err := b.stream(q, func(row map[string]json.RawMessage) error {
		var anon bool
		_ = json.Unmarshal(row["is_anonymous"], &anon) // null -> false
		if anon {
			skippedAnon++
			return nil
		}
		str := func(k string) string {
			var s string
			_ = json.Unmarshal(row[k], &s) // null -> ""
			return s
		}
		rec := core.NewRecord(coll)
		rec.Set("id", str("id"))
		rec.SetEmail(str("email"))
		rec.SetEmailVisibility(false)
		rec.SetVerified(!isNull(row["email_confirmed_at"]) || !isNull(row["phone_confirmed_at"]))
		if h := str("encrypted_password"); strings.HasPrefix(h, "$2") {
			rec.SetRaw("password", &core.PasswordFieldValue{Hash: h})
			bcryptN++
		} else {
			label := str("email")
			if label == "" {
				label = str("phone")
			}
			rec.SetRandomPassword()
			if h == "" { // OAuth / OTP only on Supabase too: they keep signing in that way
				noPw++
				b.rep.NoPassword = append(b.rep.NoPassword, label)
			} else { // argon2 / scrypt: the old password cannot be verified by PocketBase
				rec.Set("password_reset_required", true)
				reset++
				b.rep.ResetUsers = append(b.rep.ResetUsers, label+" ("+strings.SplitN(h[1:], "$", 2)[0]+" hash)")
			}
		}
		rec.RefreshTokenKey()
		rec.Set("phone", str("phone"))
		rec.Set("user_metadata", types.JSONRaw(row["raw_user_meta_data"]))
		rec.Set("app_metadata", types.JSONRaw(row["raw_app_meta_data"]))
		for k, f := range map[string]string{"created_at": "created", "updated_at": "updated", "banned_until": "banned_until", "last_sign_in_at": "last_sign_in_at"} {
			if s := str(k); s != "" {
				if dt, err := parseTime(s); err == nil {
					rec.SetRaw(f, dt)
				}
			}
		}
		for _, fp := range merged {
			if err := b.setValue(rec, fp, row["p__"+fp.PGName]); err != nil {
				return err
			}
		}
		imported++
		return bt.add(rec)
	})
	if err != nil {
		return err
	}
	if err := bt.flush(); err != nil {
		return err
	}
	if b.users.Table != nil {
		b.users.Loaded = int64(imported)
	}
	stats["imported"], stats["skippedAnonymous"], stats["bcryptPreserved"], stats["passwordResetRequired"], stats["withoutPassword"] = imported, skippedAnon, bcryptN, reset, noPw
	b.rep.Users = stats
	detail := fmt.Sprintf("%d keep their password", bcryptN)
	if noPw > 0 {
		detail += fmt.Sprintf(", %d OAuth/OTP only", noPw)
	}
	if reset > 0 {
		detail += fmt.Sprintf(", %d must reset", reset)
	}
	if skippedAnon > 0 {
		detail += fmt.Sprintf(", %d anonymous skipped", skippedAnon)
	}
	fmt.Printf("  %-18s %9s imported (%s)\n", "users", fmtInt(int64(imported)), detail)

	// identities -> _externalAuths
	b.rep.Externals = map[string]int{}
	return b.stream(`select row_to_json(x)::text from (select i.user_id, i.provider, i.provider_id from auth.identities i
		join auth.users u on u.id = i.user_id where i.provider not in ('email', 'phone') and not u.is_anonymous) x`, func(row map[string]json.RawMessage) error {
		var uid, prov, pid string
		_ = json.Unmarshal(row["user_id"], &uid) // text columns from row_to_json
		_ = json.Unmarshal(row["provider"], &prov)
		_ = json.Unmarshal(row["provider_id"], &pid)
		pbProv, ok := b.provider[prov]
		if !ok {
			pbProv = prov
		}
		ea := core.NewExternalAuth(b.app)
		ea.SetCollectionRef(coll.Id)
		ea.SetRecordRef(uid)
		ea.SetProvider(pbProv)
		ea.SetProviderId(pid)
		if err := b.app.Save(ea); err != nil {
			return fmt.Errorf("externalAuth %s/%s: %w", prov, pid, err)
		}
		b.rep.Externals[prov+" -> "+pbProv]++
		return nil
	})
}

func (b *Builder) importOrphanObjects() error {
	type obj struct {
		ID, Bucket, Name, Owner, Mime, Created string
		Size                                   float64
	}
	byBucket := map[string][]obj{}
	err := b.stream(`select row_to_json(x)::text from (select id, bucket_id, name, owner_id, metadata->>'mimetype' as mime,
		(metadata->>'size')::float8 as size, created_at from storage.objects order by created_at) x`, func(row map[string]json.RawMessage) error {
		var o obj
		_ = json.Unmarshal(row["id"], &o.ID)
		_ = json.Unmarshal(row["bucket_id"], &o.Bucket)
		_ = json.Unmarshal(row["name"], &o.Name)
		_ = json.Unmarshal(row["owner_id"], &o.Owner)
		_ = json.Unmarshal(row["mime"], &o.Mime)
		_ = json.Unmarshal(row["size"], &o.Size)
		_ = json.Unmarshal(row["created_at"], &o.Created)
		if b.linked[o.Bucket+"/"+o.Name] || strings.HasSuffix(o.Name, ".emptyFolderPlaceholder") {
			return nil
		}
		byBucket[o.Bucket] = append(byBucket[o.Bucket], o)
		return nil
	})
	if err != nil {
		return err
	}
	users := b.users.Coll
	buckets := make([]string, 0, len(byBucket))
	for k := range byBucket {
		buckets = append(buckets, k)
	}
	sort.Strings(buckets)
	for _, bk := range buckets {
		objs := byBucket[bk]
		binfo := b.bucket(bk)
		name := "storage_" + regexp.MustCompile(`\W`).ReplaceAllString(bk, "_")
		c := core.NewBaseCollection(name)
		configureID(c, "uuid")
		c.Fields.Add(
			&core.TextField{Name: "path", Required: true},
			&core.FileField{Name: "file", MaxSelect: 1, MaxSize: roundUpMB(binfo.MaxSize), Protected: !binfo.Public},
			&core.RelationField{Name: "owner", CollectionId: users.Id, MaxSelect: 1},
			&core.TextField{Name: "mimetype"}, &core.NumberField{Name: "size"},
			&core.DateField{Name: "created_at"},
		)
		c.AddIndex("idx_"+name+"_path", true, "`path`", "")
		// rules from storage.objects policies for this bucket
		var pols []Policy
		for _, p := range b.a.Policies {
			if p.Schema == "storage" && p.Table == "objects" {
				pols = append(pols, p)
			}
		}
		rc := &ruleCtx{table: &Table{Name: "objects"}, bucket: bk,
			fieldOf: func(col string) (string, bool) {
				switch col {
				case "owner_id", "owner":
					return "owner", true
				case "name":
					return "path", true
				}
				return "", false
			}, relTo: func(string) string { return "" }}
		rules := buildRules(rc, true, pols)
		cp := &CollPlan{PGName: "storage.objects[" + bk + "]", Name: name, IdMode: "uuid", Rules: rules, Coll: c, Rows: int64(len(objs))}
		applyRules(c, rules, false)
		if binfo.Public {
			c.ViewRule = types.Pointer("") // public bucket: objects readable by URL
			rules["SELECT"].Note = "public bucket -> viewRule public"
		}
		if err := b.app.Save(c); err != nil {
			return err
		}
		for _, o := range objs {
			rec := core.NewRecord(c)
			rec.Set("id", o.ID)
			rec.Set("path", o.Name)
			fn := path.Base(o.Name)
			rec.SetRaw("file", fn)
			rec.Set("owner", o.Owner)
			rec.Set("mimetype", o.Mime)
			rec.Set("size", o.Size)
			if dt, err := parseTime(o.Created); err == nil {
				rec.SetRaw("created_at", dt)
			}
			if err := b.app.SaveNoValidate(rec); err != nil {
				return err
			}
			b.jobs = append(b.jobs, FileJob{CollID: c.Id, RecID: o.ID, Bucket: bk, Object: o.Name, Filename: fn})
			cp.Loaded++
		}
		b.rep.Storage = append(b.rep.Storage, cp)
		word := "files"
		if len(objs) == 1 {
			word = "file"
		}
		fmt.Printf("  %-18s %9s %s not linked to a table (bucket %s)\n", name, fmtInt(int64(len(objs))), word, bk)
	}
	return nil
}

func (b *Builder) copyFiles() error {
	if b.skipFiles {
		pending := 0
		for _, j := range b.jobs {
			if j.Data == nil {
				pending++
			}
		}
		if pending > 0 {
			b.rep.Files["skipped"] = pending
			fmt.Printf("  %-18s %9s not copied (--skip-files): records keep the file names, content is missing\n", "files", fmtInt(int64(pending)))
		}
		var inline []FileJob
		for _, j := range b.jobs {
			if j.Data != nil {
				inline = append(inline, j)
			}
		}
		b.jobs = inline
	}
	client := b.http
	if client == nil {
		client = http.DefaultClient
	}
	fsys, err := b.app.NewFilesystem()
	if err != nil {
		return err
	}
	defer fsys.Close()
	var mu sync.Mutex
	var bytesN int64
	var errs []string
	fail := func(what string, err error) {
		mu.Lock()
		errs = append(errs, what+": "+err.Error())
		mu.Unlock()
	}
	ch := make(chan FileJob)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				data, what := j.Data, j.CollID+"/"+j.RecID+"/"+j.Filename
				if data == nil {
					what = j.Bucket + "/" + j.Object
					segs := strings.Split(j.Object, "/")
					for i := range segs {
						segs[i] = url.PathEscape(segs[i])
					}
					var err error
					if data, err = fetchObject(client, b.storageURL+"/object/"+j.Bucket+"/"+strings.Join(segs, "/"), b.serviceKey); err != nil {
						fail(what, err) // nothing is stored: a partial file would look complete
						continue
					}
				}
				if err := fsys.Upload(data, j.CollID+"/"+j.RecID+"/"+j.Filename); err != nil {
					fail(what, err)
					continue
				}
				mu.Lock()
				bytesN += int64(len(data))
				mu.Unlock()
			}
		}()
	}
	for _, j := range b.jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	sort.Strings(errs)
	if b.rep.Files == nil {
		b.rep.Files = map[string]any{}
	}
	b.rep.Files["copied"], b.rep.Files["bytes"], b.rep.Files["errors"] = len(b.jobs)-len(errs), bytesN, errs
	fmt.Printf("  %-18s %9s copied (%s, %d errors)\n", "files", fmtInt(int64(len(b.jobs)-len(errs))), fmtBytes(bytesN), len(errs))
	return nil
}

// fetchBackoff is the pause before the second download attempt (doubled before the third).
var fetchBackoff = time.Second

// fetchObject downloads one Storage object completely. A response that fails, ends early or is shorter than
// announced is an error, so a partial file is never stored as if it were whole. Failures that can pass
// (network errors, cut connections, HTTP 429 and 5xx) are tried three times; a timeout is not retried.
func fetchObject(client *http.Client, objURL, key string) ([]byte, error) {
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			time.Sleep(time.Duration(attempt-1) * fetchBackoff)
		}
		data, retry, err := fetchOnce(client, objURL, key)
		if err == nil {
			return data, nil
		}
		last = err
		if !retry {
			break
		}
	}
	return nil, last
}

func fetchOnce(client *http.Client, objURL, key string) (data []byte, retry bool, err error) {
	req, err := http.NewRequest("GET", objURL, nil)
	if err != nil {
		return nil, false, err
	}
	setKeyHeaders(req.Header, key)
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, !ue.Timeout(), ue.Err // without the URL: the caller names the object
		}
		return nil, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err = io.ReadAll(resp.Body)
	if err != nil {
		var ne net.Error
		timeout := errors.As(err, &ne) && ne.Timeout()
		return nil, !timeout, fmt.Errorf("download stopped after %s: %w", fmtBytes(int64(len(data))), err)
	}
	if resp.ContentLength >= 0 && int64(len(data)) != resp.ContentLength {
		return nil, true, fmt.Errorf("incomplete download: %d of %d bytes", len(data), resp.ContentLength)
	}
	return data, false, nil
}

// pbFieldOf maps another table's column to its PocketBase collection and field name (for join rules).
func (b *Builder) pbFieldOf(table, col string) (string, string, bool) {
	cp := b.byPG[strings.TrimPrefix(table, "public.")]
	if cp == nil || cp.Table == nil {
		return "", "", false
	}
	if t := cp.Table; len(t.PK) == 1 && t.PK[0] == col && cp.IdMode != "hash" {
		return cp.Name, "id", true
	}
	if f := cp.field(col); f != nil && f.Kind != "skip" {
		return cp.Name, f.Name, true
	}
	return "", "", false
}

func applyRules(c *core.Collection, rules map[string]*RuleResult, keepCreateDelete bool) {
	ptr := func(r *RuleResult) *string {
		if r.Status == "locked" {
			return nil
		}
		return types.Pointer(r.Rule)
	}
	c.ListRule, c.ViewRule, c.UpdateRule = ptr(rules["SELECT"]), ptr(rules["SELECT"]), ptr(rules["UPDATE"])
	if !keepCreateDelete {
		c.CreateRule, c.DeleteRule = ptr(rules["INSERT"]), ptr(rules["DELETE"])
	}
}

func (b *Builder) indexesAndRules() error {
	for _, cp := range b.plans {
		c := cp.Coll
		t := cp.Table
		colName := func(pg string) (string, bool) {
			if len(t.PK) == 1 && t.PK[0] == pg && cp.IdMode != "hash" {
				return "id", true
			}
			if f := cp.field(pg); f != nil && f.Kind != "skip" {
				return f.Name, true
			}
			return "", false
		}
		mapCols := func(cols []string) ([]string, bool) {
			var out []string
			for _, c := range cols {
				n, ok := colName(c)
				if !ok {
					return nil, false
				}
				expr := "`" + n + "`"
				if col := t.Col(c); col != nil && col.TypName == "citext" {
					expr += " COLLATE NOCASE"
				}
				out = append(out, expr)
			}
			return out, true
		}
		prefix := "idx_" + cp.Name + "_"
		if cp.IdMode == "hash" && len(t.PK) > 0 {
			if cols, ok := mapCols(t.PK); ok {
				c.AddIndex(prefix+"pk", true, strings.Join(cols, ", "), "")
			}
		}
		for i, u := range t.Uniques {
			cols, ok := mapCols(u)
			if !ok {
				continue
			}
			where := ""
			if len(cols) == 1 { // Postgres UNIQUE allows many NULLs; in PocketBase NULL is '' -> partial index
				where = strings.TrimSuffix(cols[0], " COLLATE NOCASE") + " != ''"
			}
			c.AddIndex(fmt.Sprintf("%suq%d", prefix, i), true, strings.Join(cols, ", "), where)
		}
		for _, ix := range t.Indexes {
			if ix.Expr {
				continue
			}
			cols, ok := mapCols(ix.Cols)
			if !ok {
				continue
			}
			c.AddIndex(prefix+ix.Name, ix.Unique, strings.Join(cols, ", "), normalizeExpr(ix.Where))
		}
		// rules
		var pols []Policy
		for _, p := range b.a.Policies {
			if p.Schema == "public" && p.Table == t.Name {
				pols = append(pols, p)
			}
		}
		rc := &ruleCtx{table: t, fieldOf: colName, fieldIn: b.pbFieldOf, relTo: func(col string) string {
			if f := cp.field(col); f != nil && f.Kind == "relation" {
				return f.RelPG
			}
			return ""
		}}
		rc.meDefault = func(col string) bool { f := cp.field(col); return f != nil && f.DefaultMe }
		cp.Rules = buildRules(rc, t.RLS, pols)
		applyRules(c, cp.Rules, cp.Users)
		if err := b.app.Save(c); err != nil {
			return fmt.Errorf("indexes/rules %s: %w", cp.Name, err)
		}
	}
	return nil
}

func (b *Builder) views() {
	for _, v := range b.a.Views {
		res := map[string]any{"name": v.Name, "postgres": strings.TrimSpace(v.ViewDef)}
		sqlite, err := pgViewToSQLite(v.ViewDef)
		if err == nil {
			res["sqlite"] = sqlite
			c := core.NewViewCollection(v.Name)
			c.ViewQuery = sqlite
			c.ListRule, c.ViewRule = nil, nil
			err = b.app.Save(c)
		}
		if err != nil {
			res["status"], res["error"] = "manual", err.Error()
			fmt.Printf("  %-18s not created: %v (recreate it by hand)\n", "view "+v.Name, err)
		} else {
			res["status"] = "created (locked, review rules)"
			fmt.Printf("  %-18s created (locked to superusers, review its rules)\n", "view "+v.Name)
		}
		b.rep.Views = append(b.rep.Views, res)
	}
}

func (b *Builder) hooks(outDir, authURL string) error {
	var sb strings.Builder
	sb.WriteString("/// <reference path=\"../pb_data/types.d.ts\" />\n// Generated by sb2pb (Supabase -> PocketBase). Review before deploying.\n\n")
	for _, cp := range b.plans {
		var lines []string
		for _, fp := range cp.Fields {
			if fp.DefaultLit == nil || fp.Kind == "skip" {
				continue
			}
			v, _ := json.Marshal(fp.DefaultLit)
			lines = append(lines, fmt.Sprintf("  if (!e.record.get(%q)) e.record.set(%q, %s) // DEFAULT from %s.%s", fp.Name, fp.Name, v, cp.PGName, fp.PGName))
		}
		if len(lines) > 0 {
			fmt.Fprintf(&sb, "// Postgres column DEFAULTs (PocketBase has no column defaults)\nonRecordCreate((e) => {\n%s\n  e.next()\n}, %q)\n\n", strings.Join(lines, "\n"), cp.Name)
			b.rep.Hooks = append(b.rep.Hooks, "defaults: "+cp.Name)
		}
		var me []string
		for _, fp := range cp.Fields {
			if fp.DefaultMe && fp.Kind != "skip" {
				me = append(me, fp.Name)
			}
		}
		if len(me) > 0 {
			var set []string
			for _, f := range me {
				set = append(set, fmt.Sprintf("    if (!e.record.get(%q)) e.record.set(%q, e.auth.id)", f, f))
			}
			usersName := "users"
			if b.users != nil && b.users.Coll != nil {
				usersName = b.users.Coll.Name
			}
			fmt.Fprintf(&sb, "// Postgres DEFAULT auth.uid() on %s: apps can leave %s out and get the signed-in user, as on Supabase.\n"+
				"// This runs after the API rule check, so rules accept an empty value here (%s = \"\" || %s = @request.auth.id).\n"+
				"onRecordCreateRequest((e) => {\n  if (e.auth && e.auth.collection().name === %q) {\n%s\n  }\n  e.next()\n}, %q)\n\n",
				cp.PGName, strings.Join(me, ", "), me[0], me[0], usersName, strings.Join(set, "\n"), cp.Name)
			b.rep.Hooks = append(b.rep.Hooks, "DEFAULT auth.uid(): "+cp.Name+"."+strings.Join(me, ", "))
		}
	}
	authLit, _ := json.Marshal(authURL)
	sb.WriteString(strings.Replace(`// Session continuity: exchange a still-valid Supabase access token for a PocketBase auth token.
// Verification is delegated to Supabase (GET /auth/v1/user), as recommended by Supabase docs.
routerAdd("POST", "/api/supabase/exchange", (e) => {
  const token = (e.requestInfo().body || {}).access_token || ""
  if (!token) throw new BadRequestError("missing access_token")
  const res = $http.send({
    method: "GET",
    url: ($os.getenv("SUPABASE_AUTH_URL") || __DEFAULT_AUTH_URL__) + "/user",
    headers: { "Authorization": "Bearer " + token, "apikey": $os.getenv("SUPABASE_PUBLISHABLE_KEY") || $os.getenv("SUPABASE_ANON_KEY") },
    timeout: 10,
  })
  if (res.statusCode !== 200 || !res.json || !res.json.id) throw new UnauthorizedError("invalid Supabase token")
  let record
  try { record = e.app.findRecordById("users", res.json.id) } catch (_) { throw new NotFoundError("user was not migrated") }
  return $apis.recordAuthResponse(e, record, "supabase_exchange")
})

`, "__DEFAULT_AUTH_URL__", string(authLit), 1))
	b.rep.Hooks = append(b.rep.Hooks, "route: POST /api/supabase/exchange")
	for _, f := range b.a.Functions {
		if f.Kind != "f" || f.Name == "handle_new_user" {
			continue
		}
		access := "SECURITY INVOKER: it ran with the caller's RLS. A hook bypasses API rules, so check e.auth and access yourself."
		if f.SecDef {
			access = "SECURITY DEFINER: it ignored RLS. A hook also bypasses API rules; keep any checks the function made."
		}
		fmt.Fprintf(&sb, "// TODO: port Postgres function public.%s(%s) [%s]\n// %s\nrouterAdd(\"POST\", \"/api/rpc/%s\", (e) => {\n  throw new ApiError(501, \"rpc %s not ported yet\")\n})\n\n", f.Name, f.Args, f.Lang, access, f.Name, f.Name)
		b.rep.Hooks = append(b.rep.Hooks, "stub: POST /api/rpc/"+f.Name)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "supabase_migration.pb.js"), []byte(sb.String()), 0o644)
}
