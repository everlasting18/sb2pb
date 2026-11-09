package app

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

type Column struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"` // format_type
	TypName    string   `json:"typname"`
	TypType    string   `json:"typtype"` // b=base, e=enum
	ElemName   string   `json:"elem,omitempty"`
	ElemType   string   `json:"elemtype,omitempty"`
	NotNull    bool     `json:"notNull"`
	Default    string   `json:"default,omitempty"`
	Generated  bool     `json:"generated,omitempty"`
	EnumValues []string `json:"enum,omitempty"`
	// data profiling
	MaxAbs     float64 `json:"maxAbs,omitempty"`
	MaxAbsText string  `json:"maxAbsText,omitempty"`
	NullCount  int64   `json:"nullCount,omitempty"`
	EmptyCount int64   `json:"emptyCount,omitempty"`
	Domain     string  `json:"domain,omitempty"`
	GeoSub     string  `json:"geoSub,omitempty"`
	MaxLen     int64   `json:"maxLen,omitempty"`
	MaxBytes   int64   `json:"maxBytes,omitempty"`
	NonFinite  int64   `json:"nonFinite,omitempty"`
	Lossy      int64   `json:"lossy,omitempty"`
	Infinite   int64   `json:"infinite,omitempty"`
	OutOfRange int64   `json:"outOfRange,omitempty"`
	BadPK      int64   `json:"badPK,omitempty"`
	Identity   bool    `json:"identity,omitempty"`
	// storage link detection
	LinkBucket string `json:"linkBucket,omitempty"`
	// set to now() on every UPDATE by this trigger (moddatetime or equivalent)
	Touch         string `json:"touch,omitempty"`
	TouchOnInsert bool   `json:"-"`
	NonASCII      bool   `json:"nonAscii,omitempty"`
	LinkRatio     float64
}

type FK struct {
	Cols     []string `json:"cols"`
	RefTable string   `json:"ref"` // schema.table
	RefCols  []string `json:"refCols"`
	OnDelete string   `json:"onDelete"`
}

type Index struct {
	Name   string   `json:"name"`
	Unique bool     `json:"unique"`
	Expr   bool     `json:"expr"`
	Where  string   `json:"where,omitempty"`
	Cols   []string `json:"cols"`
	Def    string   `json:"def"`
}

type Table struct {
	OID     uint32     `json:"-"`
	Name    string     `json:"name"`
	Kind    string     `json:"kind"`
	RLS     bool       `json:"rls"`
	Rows    int64      `json:"rows"`
	Columns []*Column  `json:"columns"`
	PK      []string   `json:"pk"`
	FKs     []FK       `json:"fks"`
	Uniques [][]string `json:"uniques"`
	Checks  []string   `json:"checks"`
	Indexes []Index    `json:"indexes"`
	ViewDef string     `json:"viewDef,omitempty"`
	// large tables are profiled on a TABLESAMPLE instead of a full scan
	EstRows   int64   `json:"estRows,omitempty"`
	Sampled   bool    `json:"sampled,omitempty"`
	SamplePct float64 `json:"samplePct,omitempty"`
	Writes    int64   `json:"writes,omitempty"`
}

func (t *Table) Col(name string) *Column {
	for _, c := range t.Columns {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (t *Table) FKFor(col string) *FK {
	for i := range t.FKs {
		if len(t.FKs[i].Cols) == 1 && t.FKs[i].Cols[0] == col {
			return &t.FKs[i]
		}
	}
	return nil
}

type Policy struct {
	Schema, Table, Name, Permissive, Cmd, Qual, Check string
	Roles                                             []string
}

type Fn struct {
	Name, Lang, Args, Kind string
	SecDef                 bool // SECURITY DEFINER (otherwise it ran with the caller's RLS)
}

// Trig is a trigger on a public table (or calling a public function). Touch names the column it sets to now() on UPDATE
// (moddatetime or a one-line "NEW.col = now()" function); those become autodate(onUpdate) fields instead of manual work.
type Trig struct {
	Name, Schema, Table, Func string
	Touch                     string `json:"touch,omitempty"`
	TouchOnInsert             bool   `json:"-"`
}

type Bucket struct {
	ID      string `json:"id"`
	Public  bool   `json:"public"`
	Objects int64  `json:"objects"`
	Bytes   int64  `json:"bytes"`
	MaxSize int64  `json:"maxSize"`
}

type AuthStats struct {
	Total, Anonymous, PhoneOnly, Bcrypt, Argon2, FbScrypt, NoPassword, Banned, Confirmed int64
	Providers                                                                            map[string]int64
	MFA                                                                                  map[string]int64
}

type Finding struct {
	Level string `json:"level"` // ok | warn | block | info
	Area  string `json:"area"`
	Msg   string `json:"msg"`
}

type Analysis struct {
	Tables     []*Table  `json:"tables"`
	Views      []*Table  `json:"views"`
	Policies   []Policy  `json:"policies"`
	Functions  []Fn      `json:"functions"`
	Triggers   []Trig    `json:"triggers"`
	Extensions []string  `json:"extensions"`
	Auth       AuthStats `json:"auth"`
	Buckets    []Bucket  `json:"buckets"`
	DBSize     int64     `json:"dbSize"`
	PGVersion  string    `json:"pgVersion"`
	Findings   []Finding `json:"findings"`
	HasAuth    bool      `json:"hasAuth"`
	HasStorage bool      `json:"hasStorage"`
	Realtime   []string  `json:"realtimeTables"`
	CronJobs   int64     `json:"cronJobs"`
	Writes     int64     `json:"writes"`
	StatsSecs  float64   `json:"statsSeconds"`
	Collation  string    `json:"collation"`
	ReadOnly   string    `json:"readOnly"`
}

type AnalyzeOptions struct {
	SampleRows int64 // tables with more (estimated) rows are profiled on a sample; 0 = always full scan
}

var hasStorageSchema = true

func (a *Analysis) Table(name string) *Table {
	for _, t := range a.Tables {
		if t.Name == name {
			return t
		}
	}
	return nil
}

func (a *Analysis) add(level, area, msg string, args ...any) {
	a.Findings = append(a.Findings, Finding{level, area, fmt.Sprintf(msg, args...)})
}

func qi(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func analyze(ctx context.Context, db *pgx.Conn, opts AnalyzeOptions) (*Analysis, error) {
	a := &Analysis{}
	_ = db.QueryRow(ctx, "show server_version").Scan(&a.PGVersion)
	_ = db.QueryRow(ctx, "show transaction_read_only").Scan(&a.ReadOnly)
	_ = db.QueryRow(ctx, "select pg_database_size(current_database())").Scan(&a.DBSize)
	_ = db.QueryRow(ctx, "select datcollate from pg_database where datname = current_database()").Scan(&a.Collation)
	_ = db.QueryRow(ctx, "select to_regclass('auth.users') is not null, to_regclass('storage.objects') is not null").Scan(&a.HasAuth, &a.HasStorage)
	hasStorageSchema = a.HasStorage
	if r, err := db.Query(ctx, "select tablename::text from pg_publication_tables where pubname = 'supabase_realtime' order by 1"); err == nil {
		for r.Next() {
			var n string
			if err := r.Scan(&n); err != nil {
				r.Close()
				return nil, fmt.Errorf("reading realtime tables: %w", err)
			}
			a.Realtime = append(a.Realtime, n)
		}
		r.Close()
	}
	var hasCron bool
	_ = db.QueryRow(ctx, "select to_regclass('cron.job') is not null").Scan(&hasCron)
	if hasCron {
		_ = db.QueryRow(ctx, "select count(*) from cron.job").Scan(&a.CronJobs)
	}
	_ = db.QueryRow(ctx, `select coalesce(sum(n_tup_ins + n_tup_upd + n_tup_del), 0)::bigint,
		extract(epoch from now() - coalesce((select stats_reset from pg_stat_database where datname = current_database()), pg_postmaster_start_time()))::float8
		from pg_stat_user_tables where schemaname = 'public'`).Scan(&a.Writes, &a.StatsSecs)

	rows, err := db.Query(ctx, `select c.oid, c.relname, c.relkind::text, c.relrowsecurity, coalesce(pg_get_viewdef(c.oid, true), '')
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where n.nspname = 'public' and c.relkind in ('r','p','v','m') order by c.relname`)
	if err != nil {
		return nil, err
	}
	var all []*Table
	for rows.Next() {
		t := &Table{}
		var vd string
		if err := rows.Scan(&t.OID, &t.Name, &t.Kind, &t.RLS, &vd); err != nil {
			return nil, err
		}
		if t.Kind == "v" || t.Kind == "m" {
			t.ViewDef = vd
		}
		all = append(all, t)
	}
	rows.Close()

	for _, t := range all {
		crows, err := db.Query(ctx, `select a.attname, format_type(a.atttypid, a.atttypmod), coalesce(bt.typname, t.typname)::text,
			coalesce(bt.typtype, t.typtype)::text, coalesce(et.typname, ''), coalesce(et.typtype::text, ''), a.attnotnull or coalesce(t.typnotnull, false),
			coalesce(pg_get_expr(d.adbin, d.adrelid), ''), a.attgenerated <> '',
			case when coalesce(bt.typtype, t.typtype) = 'e' then coalesce(bt.oid, t.oid) when et.typtype = 'e' then et.oid else 0 end,
			case when t.typtype = 'd' then t.typname::text else '' end, a.attidentity <> ''
			from pg_attribute a join pg_type t on t.oid = a.atttypid
			left join pg_type bt on t.typtype = 'd' and bt.oid = t.typbasetype
			left join pg_type et on et.oid = coalesce(bt.typelem, t.typelem) and coalesce(bt.typcategory, t.typcategory) = 'A'
			left join pg_attrdef d on d.adrelid = a.attrelid and d.adnum = a.attnum
			where a.attrelid = $1 and a.attnum > 0 and not a.attisdropped order by a.attnum`, t.OID)
		if err != nil {
			return nil, err
		}
		type enumRef struct {
			c   *Column
			oid uint32
		}
		var enums []enumRef
		for crows.Next() {
			c := &Column{}
			var eoid uint32
			if err := crows.Scan(&c.Name, &c.Type, &c.TypName, &c.TypType, &c.ElemName, &c.ElemType, &c.NotNull, &c.Default, &c.Generated, &eoid, &c.Domain, &c.Identity); err != nil {
				return nil, err
			}
			t.Columns = append(t.Columns, c)
			if eoid != 0 {
				enums = append(enums, enumRef{c, eoid})
			}
		}
		crows.Close()
		for _, e := range enums {
			erows, _ := db.Query(ctx, "select enumlabel from pg_enum where enumtypid = $1 order by enumsortorder", e.oid)
			for erows.Next() {
				var l string
				if err := erows.Scan(&l); err != nil {
					erows.Close()
					return nil, fmt.Errorf("reading enum values: %w", err)
				}
				e.c.EnumValues = append(e.c.EnumValues, l)
			}
			erows.Close()
		}
		if t.Kind == "v" || t.Kind == "m" {
			a.Views = append(a.Views, t)
			continue
		}

		_ = db.QueryRow(ctx, "select reltuples::bigint from pg_class where oid = $1", t.OID).Scan(&t.EstRows)
		_ = db.QueryRow(ctx, "select coalesce(n_tup_ins + n_tup_upd + n_tup_del, 0) from pg_stat_user_tables where relid = $1", t.OID).Scan(&t.Writes)
		if opts.SampleRows > 0 && t.EstRows > opts.SampleRows {
			t.Rows, t.Sampled = t.EstRows, true
			t.SamplePct = float64(int(1000*float64(opts.SampleRows)/float64(t.EstRows))) / 10
			if t.SamplePct < 0.1 {
				t.SamplePct = 0.1
			}
		} else {
			if err := db.QueryRow(ctx, "select count(*) from public."+qi(t.Name)).Scan(&t.Rows); err != nil {
				return nil, fmt.Errorf("counting rows of %s: %w", t.Name, err)
			}
		}

		kr, err := db.Query(ctx, `select contype::text,
			coalesce((select array_agg(attname::text order by k.ord) from unnest(conkey) with ordinality k(n, ord) join pg_attribute on attrelid = conrelid and attnum = k.n), '{}'),
			coalesce(confrelid::regclass::text, ''),
			coalesce((select array_agg(attname::text order by k.ord) from unnest(confkey) with ordinality k(n, ord) join pg_attribute on attrelid = confrelid and attnum = k.n), '{}'),
			confdeltype::text, pg_get_constraintdef(oid)
			from pg_constraint where conrelid = $1`, t.OID)
		if err != nil {
			return nil, err
		}
		for kr.Next() {
			var typ, ref, del, def string
			var cols, refCols []string
			if err := kr.Scan(&typ, &cols, &ref, &refCols, &del, &def); err != nil {
				return nil, err
			}
			switch typ {
			case "p":
				t.PK = cols
			case "u":
				t.Uniques = append(t.Uniques, cols)
			case "c":
				t.Checks = append(t.Checks, def)
			case "f":
				if !strings.Contains(ref, ".") {
					ref = "public." + ref
				}
				t.FKs = append(t.FKs, FK{Cols: cols, RefTable: ref, RefCols: refCols, OnDelete: del})
			}
		}
		kr.Close()

		ir, err := db.Query(ctx, `select i.relname, x.indisunique, x.indexprs is not null, coalesce(pg_get_expr(x.indpred, x.indrelid), ''),
			coalesce((select array_agg(a.attname::text order by k.ord) from unnest(x.indkey::int2[]) with ordinality k(n, ord) join pg_attribute a on a.attrelid = x.indrelid and a.attnum = k.n), '{}'),
			pg_get_indexdef(x.indexrelid)
			from pg_index x join pg_class i on i.oid = x.indexrelid
			where x.indrelid = $1 and not exists (select 1 from pg_constraint c where c.conindid = x.indexrelid)`, t.OID)
		if err != nil {
			return nil, err
		}
		for ir.Next() {
			var ix Index
			if err := ir.Scan(&ix.Name, &ix.Unique, &ix.Expr, &ix.Where, &ix.Cols, &ix.Def); err != nil {
				return nil, err
			}
			t.Indexes = append(t.Indexes, ix)
		}
		ir.Close()

		// data profiling for the columns whose mapping depends on actual data
		for _, c := range t.Columns {
			profileColumn(ctx, db, t, c)
		}
		if len(t.Columns) > 0 {
			var parts []string
			for _, c := range t.Columns {
				parts = append(parts, fmt.Sprintf("count(*) filter (where %s is null)", qi(c.Name)))
			}
			dst := make([]any, len(t.Columns))
			for i, c := range t.Columns {
				dst[i] = &c.NullCount
			}
			if err := db.QueryRow(ctx, fmt.Sprintf("select %s from %s", strings.Join(parts, ", "), sourceOf(t))).Scan(dst...); err != nil {
				a.add("warn", "Data", "could not profile %s (field limits use defaults): %v", t.Name, err)
			}
		}
		a.Tables = append(a.Tables, t)
	}

	prow, err := db.Query(ctx, `select schemaname::text, tablename::text, policyname::text, permissive, roles::text[], cmd, coalesce(qual, ''), coalesce(with_check, '')
		from pg_policies where schemaname in ('public', 'storage') order by schemaname, tablename, policyname`)
	if err != nil {
		return nil, err
	}
	for prow.Next() {
		var p Policy
		if err := prow.Scan(&p.Schema, &p.Table, &p.Name, &p.Permissive, &p.Roles, &p.Cmd, &p.Qual, &p.Check); err != nil {
			prow.Close()
			return nil, fmt.Errorf("reading policies: %w", err)
		}
		a.Policies = append(a.Policies, p)
	}
	prow.Close()

	frow, _ := db.Query(ctx, `select p.proname::text, l.lanname::text, pg_get_function_identity_arguments(p.oid), p.prokind::text, p.prosecdef
		from pg_proc p join pg_namespace n on n.oid = p.pronamespace join pg_language l on l.oid = p.prolang where n.nspname = 'public' order by 1`)
	for frow.Next() {
		var f Fn
		if err := frow.Scan(&f.Name, &f.Lang, &f.Args, &f.Kind, &f.SecDef); err != nil {
			frow.Close()
			return nil, fmt.Errorf("reading functions: %w", err)
		}
		a.Functions = append(a.Functions, f)
	}
	frow.Close()

	trow, _ := db.Query(ctx, `select t.tgname::text, n.nspname::text, c.relname::text, pr.proname::text, pg_get_triggerdef(t.oid), coalesce(pr.prosrc, '')
		from pg_trigger t join pg_class c on c.oid = t.tgrelid join pg_namespace n on n.oid = c.relnamespace join pg_proc pr on pr.oid = t.tgfoid
		where not t.tgisinternal and (pr.pronamespace = 'public'::regnamespace or n.nspname = 'public') order by 1`)
	for trow.Next() {
		var tg Trig
		var def, src string
		if err := trow.Scan(&tg.Name, &tg.Schema, &tg.Table, &tg.Func, &def, &src); err != nil {
			trow.Close()
			return nil, fmt.Errorf("reading triggers: %w", err)
		}
		tg.Touch, tg.TouchOnInsert = touchColumn(def, tg.Func, src)
		a.Triggers = append(a.Triggers, tg)
	}
	trow.Close()
	for _, tg := range a.Triggers {
		if tg.Touch == "" || tg.Schema != "public" {
			continue
		}
		if t := a.Table(tg.Table); t != nil {
			if c := t.Col(tg.Touch); c != nil {
				c.Touch, c.TouchOnInsert = tg.Name, c.TouchOnInsert || tg.TouchOnInsert
			}
		}
	}

	erow, _ := db.Query(ctx, "select extname::text from pg_extension order by 1")
	for erow.Next() {
		var e string
		if err := erow.Scan(&e); err != nil {
			erow.Close()
			return nil, fmt.Errorf("reading extensions: %w", err)
		}
		a.Extensions = append(a.Extensions, e)
	}
	erow.Close()

	s := &a.Auth
	s.Providers, s.MFA = map[string]int64{}, map[string]int64{}
	if a.HasAuth {
		_ = db.QueryRow(ctx, `select count(*), count(*) filter (where is_anonymous), count(*) filter (where email is null and phone is not null),
		count(*) filter (where encrypted_password like '$2%'), count(*) filter (where encrypted_password like '$argon2%'),
		count(*) filter (where encrypted_password like '$fbscrypt%'), count(*) filter (where coalesce(encrypted_password, '') = '' and not is_anonymous),
		count(*) filter (where banned_until > now()), count(*) filter (where email_confirmed_at is not null) from auth.users where deleted_at is null`).
			Scan(&s.Total, &s.Anonymous, &s.PhoneOnly, &s.Bcrypt, &s.Argon2, &s.FbScrypt, &s.NoPassword, &s.Banned, &s.Confirmed)
		r2, _ := db.Query(ctx, "select provider, count(*) from auth.identities group by 1")
		for r2.Next() {
			var k string
			var n int64
			if err := r2.Scan(&k, &n); err != nil {
				r2.Close()
				return nil, fmt.Errorf("reading identities: %w", err)
			}
			s.Providers[k] = n
		}
		r2.Close()
		r3, _ := db.Query(ctx, "select factor_type::text || '/' || status::text, count(*) from auth.mfa_factors group by 1")
		for r3.Next() {
			var k string
			var n int64
			if err := r3.Scan(&k, &n); err != nil {
				r3.Close()
				return nil, fmt.Errorf("reading MFA factors: %w", err)
			}
			s.MFA[k] = n
		}
		r3.Close()
	}
	if a.HasStorage {

		br, _ := db.Query(ctx, `select b.id, b.public, count(o.id), coalesce(sum((o.metadata->>'size')::bigint), 0), coalesce(max((o.metadata->>'size')::bigint), 0)
		from storage.buckets b left join storage.objects o on o.bucket_id = b.id group by 1, 2 order by 1`)
		for br.Next() {
			var b Bucket
			if err := br.Scan(&b.ID, &b.Public, &b.Objects, &b.Bytes, &b.MaxSize); err != nil {
				br.Close()
				return nil, fmt.Errorf("reading buckets: %w", err)
			}
			a.Buckets = append(a.Buckets, b)
		}
		br.Close()
	}

	a.findings()
	return a, nil
}

func (a *Analysis) findings() {
	var rows int64
	for _, t := range a.Tables {
		rows += t.Rows
	}
	a.add("info", "Database", "PostgreSQL %s, %d tables, %d views, %d rows, %.1f MB", a.PGVersion, len(a.Tables), len(a.Views), rows, float64(a.DBSize)/1e6)
	for _, t := range a.Tables {
		if len(t.PK) == 0 {
			a.add("warn", "Schema", "%s: no primary key -> synthetic id + unique index", t.Name)
		} else if len(t.PK) > 1 {
			a.add("warn", "Schema", "%s: composite primary key (%s) -> deterministic id + unique index", t.Name, strings.Join(t.PK, ", "))
		}
		if !t.RLS {
			a.add("block", "Security", "%s: RLS disabled on Supabase (table was readable via anon key) -> locked (superuser only) until reviewed", t.Name)
		}
		for _, c := range t.Columns {
			switch {
			case c.Generated:
				a.add("warn", "Schema", "%s.%s: generated column -> dropped (recompute in hook)", t.Name, c.Name)
			case c.TypName == "int8" && c.MaxAbs > 9007199254740991:
				a.add("warn", "Data", "%s.%s: bigint values exceed 2^53 (max %s) -> stored as text to keep exact value", t.Name, c.Name, c.MaxAbsText)
			case c.TypName == "numeric":
				a.add("info", "Data", "%s.%s: numeric -> float64 number (max %.4g); exact decimals not guaranteed", t.Name, c.Name, c.MaxAbs)
			case c.TypName == "bool" && c.NullCount > 0:
				a.add("warn", "Data", "%s.%s: %d NULL booleans become false (PocketBase fields are NOT NULL)", t.Name, c.Name, c.NullCount)
			case c.LinkBucket != "":
				a.add("ok", "Storage", "%s.%s: %.0f%% of values point to bucket '%s' -> converted to file field", t.Name, c.Name, c.LinkRatio*100, c.LinkBucket)
			case c.TypName == "vector" || c.TypName == "geometry" || c.TypName == "tsvector":
				a.add("warn", "Schema", "%s.%s: %s has no PocketBase equivalent", t.Name, c.Name, c.TypName)
			}
		}
		for _, ck := range t.Checks {
			if !reCharLen.MatchString(ck) {
				a.add("warn", "Schema", "%s: CHECK %s -> not translated (move to hook)", t.Name, ck)
			}
		}
	}
	for _, v := range a.Views {
		a.add("warn", "Schema", "view %s -> SQLite view collection (rewritten automatically when possible, review required)", v.Name)
	}
	for _, f := range a.Functions {
		if f.Name == "handle_new_user" {
			continue
		}
		if f.Kind == "f" {
			a.add("warn", "Logic", "function %s(%s) [%s] -> needs rewrite as pb_hooks route (stub generated)", f.Name, f.Args, f.Lang)
		}
	}
	for _, tg := range a.Triggers {
		if tg.Touch != "" && tg.Schema == "public" {
			a.add("ok", "Logic", "trigger %s on %s.%s keeps %s current -> autodate field (onUpdate)", tg.Name, tg.Schema, tg.Table, tg.Touch)
			continue
		}
		if tg.Func == "handle_new_user" {
			a.add("ok", "Logic", "trigger %s on %s.%s (create profile on signup) -> obsolete: profiles merged into users", tg.Name, tg.Schema, tg.Table)
			continue
		}
		a.add("warn", "Logic", "trigger %s on %s.%s -> rewrite as onRecord* hook", tg.Name, tg.Schema, tg.Table)
	}
	for _, e := range a.Extensions {
		switch e {
		case "postgis", "vector", "timescaledb", "pgroonga", "pgmq":
			a.add("block", "Extensions", "extension %s installed -> no SQLite/PocketBase equivalent", e)
		case "pg_cron":
			a.add("warn", "Extensions", "pg_cron installed -> port jobs to cronAdd() in pb_hooks")
		case "pg_net", "http":
			a.add("warn", "Extensions", "%s installed (database webhooks / outbound HTTP) -> port to hooks with $http.send if used", e)
		}
	}
	s := a.Auth
	a.add("ok", "Auth", "%d users; %d bcrypt hashes import as-is (users keep their passwords)", s.Total, s.Bcrypt)
	if s.Argon2+s.FbScrypt > 0 {
		a.add("warn", "Auth", "%d users have non-bcrypt hashes (argon2/firebase-scrypt) -> must reset password", s.Argon2+s.FbScrypt)
	}
	if s.Anonymous > 0 {
		a.add("warn", "Auth", "%d anonymous users -> skipped (PocketBase has no anonymous auth)", s.Anonymous)
	}
	if s.PhoneOnly > 0 {
		a.add("warn", "Auth", "%d phone-only users -> imported, but SMS OTP login is not available in PocketBase", s.PhoneOnly)
	}
	if s.Banned > 0 {
		a.add("ok", "Auth", "%d banned users -> banned_until field + collection authRule", s.Banned)
	}
	for p, n := range s.Providers {
		if p != "email" && p != "phone" {
			a.add("ok", "Auth", "%d %s identities -> _externalAuths (re-enter client id/secret, add new redirect URL)", n, p)
		}
	}
	for k, n := range s.MFA {
		a.add("warn", "Auth", "%d MFA factors (%s) -> cannot be migrated, users must re-enroll", n, k)
	}
	a.add("warn", "Auth", "existing sessions/refresh tokens cannot move -> users re-login (or use the generated token-exchange endpoint)")
	for _, b := range a.Buckets {
		a.add("ok", "Storage", "bucket %s (%s): %d objects, %.1f KB", b.ID, map[bool]string{true: "public", false: "private"}[b.Public], b.Objects, float64(b.Bytes)/1024)
	}
}

var (
	reTouchTrig = regexp.MustCompile(`(?i)\bBEFORE ((?:INSERT OR )?UPDATE(?: OR INSERT)?) ON \S+ FOR EACH ROW EXECUTE (?:FUNCTION|PROCEDURE) (?:\w+\.)?(\w+)\(([^)]*)\)`)
	reTouchBody = regexp.MustCompile(`(?is)^\s*begin\s+new\.(\w+)\s*:?=\s*(?:now\(\)|current_timestamp|clock_timestamp\(\)|statement_timestamp\(\)|timezone\(\s*'utc'(?:::text)?\s*,\s*now\(\)\s*\))\s*;\s*return\s+new\s*;\s*end\s*;?\s*$`)
)

// touchColumn recognises "keep updated_at current" triggers: extensions.moddatetime(col), or a plpgsql function whose
// whole body is NEW.col := now(); RETURN NEW. It returns the column, and whether the trigger also fires on INSERT.
func touchColumn(triggerDef, fn, src string) (string, bool) {
	m := reTouchTrig.FindStringSubmatch(triggerDef)
	if m == nil {
		return "", false
	}
	onInsert := strings.Contains(strings.ToUpper(m[1]), "INSERT")
	if strings.EqualFold(fn, "moddatetime") {
		col := strings.Trim(strings.TrimSpace(m[3]), `'"`)
		if col != "" && !strings.Contains(col, ",") {
			return col, onInsert
		}
		return "", false
	}
	if b := reTouchBody.FindStringSubmatch(src); b != nil {
		return strings.ToLower(b[1]), onInsert
	}
	return "", false
}
