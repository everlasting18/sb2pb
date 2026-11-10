package app

// Migration readiness: read-only analysis of a Supabase project -> score, blockers, effort estimate, mapping preview.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type RItem struct {
	Severity string  `json:"severity"` // blocker | action | info | ok
	Area     string  `json:"area"`
	Title    string  `json:"title"`
	Detail   string  `json:"detail"`
	Handling string  `json:"handling"`
	Effort   float64 `json:"effortHours,omitempty"`
	Penalty  int     `json:"penalty,omitempty"`
	// Stops marks a problem that makes the migration impossible (not just incomplete): migrate refuses to start.
	Stops bool `json:"stopsMigration,omitempty"`
}

type RField struct {
	PG, PGType, PB, Kind, Note string
	Warn                       bool
}

type RRule struct {
	Cmd, Status, Rule, Reason string
	Policies                  []string
}

type RTable struct {
	PG        string   `json:"pg"`
	PB        string   `json:"pb"`
	Rows      int64    `json:"rows"`
	Sampled   bool     `json:"sampled"`
	SamplePct float64  `json:"samplePct"`
	IdMode    string   `json:"idMode"`
	RLS       bool     `json:"rls"`
	Merged    bool     `json:"mergedIntoUsers"`
	Fields    []RField `json:"fields"`
	Rules     []RRule  `json:"rules"`
	Warnings  int      `json:"warnings"`
}

type EffortLine struct {
	Label string  `json:"label"`
	Hours float64 `json:"hours"`
}

type ProviderMap struct {
	Supabase, PocketBase string
	Users                int64
	Supported            bool
}

type Readiness struct {
	ID          string        `json:"id"`
	Label       string        `json:"label"`
	Project     string        `json:"project"`
	Host        string        `json:"host"`
	Database    string        `json:"database"`
	AnalyzedAt  time.Time     `json:"analyzedAt"`
	DurationMs  int64         `json:"durationMs"`
	PGVersion   string        `json:"pgVersion"`
	Collation   string        `json:"collation"`
	Target      string        `json:"target"`
	Score       int           `json:"score"`
	Verdict     string        `json:"verdict"`
	VerdictKey  string        `json:"verdictKey"` // green | amber | red
	VerdictText string        `json:"verdictText"`
	EffortLow   float64       `json:"effortLowHours"`
	EffortHigh  float64       `json:"effortHighHours"`
	Effort      []EffortLine  `json:"effortBreakdown"`
	MigrateSecs float64       `json:"estimatedMigrationSeconds"`
	Stats       RStats        `json:"stats"`
	Items       []RItem       `json:"items"`
	Differences []RItem       `json:"behaviorDifferences"`
	Tables      []RTable      `json:"tables"`
	Storage     []RTable      `json:"storageCollections"`
	Providers   []ProviderMap `json:"providers"`
	Buckets     []Bucket      `json:"buckets"`
	Auth        AuthStats     `json:"auth"`
	Functions   []Fn          `json:"functions"`
	Triggers    []Trig        `json:"triggers"`
	Views       []string      `json:"views"`
	Extensions  []string      `json:"extensions"`
	Realtime    []string      `json:"realtimeTables"`
	Safety      []string      `json:"safety"`
	HasAuth     bool          `json:"hasAuth"`
	HasStorage  bool          `json:"hasStorage"`
}

type RStats struct {
	Tables, Views, Functions, Triggers, Policies, PoliciesAuto, PoliciesReview, PoliciesLocked int
	Rows, DBBytes, Users, UsersImportable, UsersKeepPassword, Objects, StorageBytes            int64
	Buckets                                                                                    int
	WritesPerSec                                                                               float64
}

// PocketBase's built-in OAuth2 providers (v0.41)
var pbProviders = set("apple", "bitbucket", "box", "discord", "facebook", "gitea", "gitee", "github", "gitlab", "google", "instagram2",
	"kakao", "lark", "linear", "livechat", "mailcow", "microsoft", "monday", "notion", "oidc", "oidc2", "oidc3", "patreon", "planningcenter",
	"spotify", "strava", "trakt", "twitch", "twitter", "vk", "wakatime", "yandex")

// Supabase provider -> PocketBase provider. "oidc" means: configure it as a generic OIDC provider;
// PocketBase has three slots (oidc, oidc2, oidc3), assigned in name order by resolveProviders.
var providerMap = map[string]string{"instagram": "instagram2", "azure": "microsoft", "keycloak": "oidc", "linkedin_oidc": "oidc", "slack_oidc": "oidc"}

// resolveProviders maps every Supabase identity provider to the PocketBase provider name used for
// its _externalAuths records. The readiness report and the migration use the same mapping.
func resolveProviders(providers map[string]int64) map[string]string {
	names := make([]string, 0, len(providers))
	for p := range providers {
		if p != "email" && p != "phone" {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	out := map[string]string{}
	slots := []string{"oidc", "oidc2", "oidc3"}
	for _, p := range names {
		pb, ok := providerMap[p]
		if !ok {
			pb = p
		}
		if pb == "oidc" {
			if len(slots) == 0 {
				pb = "oidc (no free slot)"
			} else {
				pb, slots = slots[0], slots[1:]
			}
		}
		out[p] = pb
	}
	return out
}

func (b *Builder) rulesPreview(cp *CollPlan) map[string]*RuleResult {
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
	return buildRules(rc, t.RLS, pols)
}

func rrules(m map[string]*RuleResult) []RRule {
	var out []RRule
	for _, cmd := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		r := m[cmd]
		out = append(out, RRule{Cmd: cmd, Status: r.Status, Rule: r.Rule, Reason: r.Err, Policies: r.Sources})
	}
	return out
}

func roundHalf(h float64) float64 { return math.Round(h*2) / 2 }

func buildReadiness(a *Analysis) *Readiness {
	b := &Builder{a: a, linked: map[string]bool{}, warnings: map[string]int{}}
	b.plan()
	r := &Readiness{Target: pocketbaseTarget, PGVersion: a.PGVersion, Collation: a.Collation, Auth: a.Auth, Buckets: a.Buckets,
		Functions: a.Functions, Triggers: a.Triggers, Extensions: a.Extensions, Realtime: a.Realtime, HasAuth: a.HasAuth, HasStorage: a.HasStorage}
	st := &r.Stats
	add := func(it RItem) { r.Items = append(r.Items, it) }
	diff := func(it RItem) { r.Differences = append(r.Differences, it) }
	for _, it := range schemaStoppers(b.plans) {
		add(it)
	}

	// ---------- tables & mapping ----------
	var nullCols, textNums, nonASCII, tsCols, nonFinite []string
	var hashedPK, generated, checks, vectorCols, geoCols, tsvectors []string
	for _, cp := range b.plans {
		t := cp.Table
		rules := b.rulesPreview(cp)
		rt := RTable{PG: t.Name, PB: cp.Name, Rows: t.Rows, Sampled: t.Sampled, SamplePct: t.SamplePct, IdMode: cp.IdMode, RLS: t.RLS, Merged: cp.Users, Rules: rrules(rules)}
		for _, fp := range cp.Fields {
			c := t.Col(fp.PGName)
			f := RField{PG: fp.PGName, PGType: fp.PGType, PB: fp.Name, Kind: fp.Kind, Note: fp.Note}
			if fp.PGName == "" {
				f.PG = "(new)"
			}
			if c != nil {
				ref := t.Name + "." + c.Name
				if c.NullCount > 0 && fp.Kind != "json" && fp.Kind != "id" && fp.Kind != "skip" && fp.Kind != "relation" && fp.Kind != "file" && fp.Kind != "bytes" {
					nullCols = append(nullCols, ref)
					f.Note = strings.TrimPrefix(f.Note+"; "+pl(c.NullCount, "NULL", "NULLs")+" → zero value", "; ")
					f.Warn = true
				}
				if fp.Kind == "text" && (c.TypName == "int8" || c.TypName == "numeric" || c.TypName == "money") {
					textNums = append(textNums, ref)
					f.Warn = true
				}
				if c.NonASCII && textTypes[c.TypName] {
					nonASCII = append(nonASCII, ref)
				}
				if timeTypes[c.TypName] {
					tsCols = append(tsCols, ref)
				}
				if c.NonFinite > 0 && fp.Kind == "number" {
					nonFinite = append(nonFinite, ref)
					f.Warn = true
				}
				if c.Generated {
					generated = append(generated, ref)
				}
				switch {
				case c.TypName == "vector" || c.TypName == "halfvec":
					vectorCols = append(vectorCols, ref)
					f.Warn = true
				case isGeo(c) && c.GeoSub != "point":
					geoCols = append(geoCols, ref)
					f.Warn = true
				case c.TypName == "tsvector":
					tsvectors = append(tsvectors, ref)
					f.Warn = true
				}
			}
			if f.Warn {
				rt.Warnings++
			}
			rt.Fields = append(rt.Fields, f)
		}
		if cp.IdMode == "hash" && len(t.PK) == 1 {
			hashedPK = append(hashedPK, t.Name)
		}
		for _, ck := range t.Checks {
			if !reCharLen.MatchString(ck) {
				checks = append(checks, t.Name+": "+ck)
			}
		}
		for _, rr := range rt.Rules {
			if len(rr.Policies) == 0 && rr.Status == "locked" {
				continue
			}
			st.Policies++
			switch rr.Status {
			case "translated":
				st.PoliciesAuto++
			case "review":
				st.PoliciesReview++
			case "locked":
				st.PoliciesLocked++
			}
		}
		r.Tables = append(r.Tables, rt)
		st.Rows += t.Rows
	}
	st.Tables, st.Views, st.DBBytes = len(a.Tables), len(a.Views), a.DBSize
	if a.StatsSecs > 0 {
		st.WritesPerSec = float64(a.Writes) / a.StatsSecs
	}

	// ---------- data ----------
	add(RItem{Severity: "ok", Area: "Data", Title: fmt.Sprintf("%s · %s import automatically", pl(int64(st.Tables), "table", "tables"), pl(st.Rows, "row", "rows")),
		Detail:   "Primary keys, foreign keys, unique constraints and indexes are mapped. UUID primary keys are kept as record ids, so links and URLs keep working.",
		Handling: "Automatic."})
	if gb := float64(a.DBSize) / 1e9; gb > 50 {
		add(RItem{Severity: "blocker", Area: "Data", Title: fmt.Sprintf("Large database (%.1f GB)", gb), Detail: "PocketBase stores everything in one SQLite file. Above ~50 GB, backups, restores and VACUUM become slow.", Handling: "Archive or split cold data before migrating.", Penalty: 25})
	} else if gb > 10 {
		add(RItem{Severity: "action", Area: "Data", Title: fmt.Sprintf("Sizeable database (%.1f GB)", gb), Detail: "Works, but plan a longer migration window and check backup times.", Handling: "Plan the cutover window.", Penalty: 5, Effort: 1})
	}
	switch {
	case a.StatsSecs > 0 && a.StatsSecs < 24*3600 && st.WritesPerSec > 50:
		// statistics were reset recently (new project, restart, bulk import): the average is not representative
		add(RItem{Severity: "info", Area: "Data", Title: "Write rate not measured", Detail: fmt.Sprintf("Postgres statistics only cover the last %s, so the average (%.0f writes/s) mostly reflects recent bulk loading.", fmtDuration(a.StatsSecs), st.WritesPerSec), Handling: "Check the write rate of normal traffic before cutover; SQLite has a single writer."})
	case st.WritesPerSec > 300:
		add(RItem{Severity: "blocker", Area: "Data", Title: fmt.Sprintf("High write rate (%.0f writes/s on average)", st.WritesPerSec), Detail: "SQLite has a single writer. Sustained heavy writes are the main case where PocketBase is the wrong tool.", Handling: "Keep Postgres for the write-heavy part, or batch writes.", Penalty: 25})
	case st.WritesPerSec > 50:
		add(RItem{Severity: "action", Area: "Data", Title: fmt.Sprintf("Moderate write rate (%.0f writes/s on average)", st.WritesPerSec), Detail: "Fine for SQLite in WAL mode, but watch peak bursts.", Handling: "Load-test peak traffic after migration.", Penalty: 5, Effort: 2})
	}
	if len(vectorCols) > 0 {
		add(RItem{Severity: "blocker", Area: "Data", Title: "pgvector columns: " + strings.Join(vectorCols, ", "), Detail: "Embeddings are copied as JSON arrays, but PocketBase has no vector index or similarity search.", Handling: "Keep a separate vector store (or Postgres) for semantic search.", Penalty: 25, Effort: 8})
	}
	if len(geoCols) > 0 {
		add(RItem{Severity: "action", Area: "Data", Title: "PostGIS shapes: " + strings.Join(geoCols, ", "), Detail: "Points become PocketBase geoPoint fields (distance filters work). Other geometries are stored as GeoJSON without spatial queries.", Handling: "Move spatial queries into hooks or the client.", Penalty: 6, Effort: 4})
	}
	if len(textNums) > 0 {
		add(RItem{Severity: "action", Area: "Data", Title: pl(int64(len(textNums)), "numeric column is", "numeric columns are") + " stored as text to keep exact values", Detail: strings.Join(textNums, ", ") + " — values exceed 2^53 or are not exact in float64.", Handling: "Filters on these fields compare as text; cast in app code where needed.", Penalty: 3, Effort: 1})
	}
	if len(nonFinite) > 0 {
		add(RItem{Severity: "action", Area: "Data", Title: "NaN / Infinity values in " + strings.Join(nonFinite, ", "), Detail: "PocketBase numbers must be finite; these values become 0.", Handling: "Map them to null-like sentinels in app logic if they matter.", Penalty: 2, Effort: 0.5})
	}
	if len(tsvectors) > 0 {
		add(RItem{Severity: "action", Area: "Data", Title: "Full-text search columns (tsvector): " + strings.Join(tsvectors, ", "), Detail: "Not migrated; Postgres full-text search has no PocketBase equivalent.", Handling: "Use `~` filters, a normalized search field, or an external search service.", Penalty: 5, Effort: 3})
	}
	if len(generated) > 0 {
		add(RItem{Severity: "action", Area: "Data", Title: "Generated columns: " + strings.Join(generated, ", "), Detail: "Current values are imported, but PocketBase will not recompute them.", Handling: "Recompute in an onRecordCreate/onRecordUpdate hook.", Penalty: 2, Effort: 0.5 * float64(len(generated))})
	}
	if len(checks) > 0 {
		add(RItem{Severity: "action", Area: "Data", Title: pl(int64(len(checks)), "CHECK constraint is", "CHECK constraints are") + " not enforced", Detail: strings.Join(checks, " · "), Handling: "Re-implement as hooks (simple length checks are converted automatically).", Penalty: 2, Effort: 0.5 * float64(len(checks))})
	}
	if len(hashedPK) > 0 {
		add(RItem{Severity: "action", Area: "Data", Title: "Text primary keys PocketBase cannot use as ids: " + strings.Join(hashedPK, ", "), Detail: "Keys containing '/', '.', spaces or reserved names get a hashed id; the original key is kept in its own unique field and foreign keys are remapped.", Handling: "Look records up by the original key field instead of the id.", Penalty: 2, Effort: 0.5})
	}
	for _, v := range a.Views {
		r.Views = append(r.Views, v.Name)
	}
	if len(a.Views) > 0 {
		add(RItem{Severity: "action", Area: "Data", Title: pl(int64(len(a.Views)), "view becomes a PocketBase view collection", "views become PocketBase view collections"), Detail: strings.Join(r.Views, ", ") + " — simple views are rewritten for SQLite automatically (casts, schema prefixes); views using Postgres-only functions are listed for manual work. Views are created locked.", Handling: "Review each view query and set its rules.", Penalty: 2 * len(a.Views), Effort: 0.5 * float64(len(a.Views))})
	}

	// ---------- auth ----------
	s := a.Auth
	if !a.HasAuth {
		add(RItem{Severity: "info", Area: "Auth", Title: "No Supabase auth schema found", Detail: "This database does not look like a Supabase project, so users are not analyzed.", Handling: "—"})
	} else {
		st.Users = s.Total
		st.UsersImportable = s.Total - s.Anonymous
		st.UsersKeepPassword = s.Bcrypt
		add(RItem{Severity: "ok", Area: "Auth", Title: fmt.Sprintf("%s of %s users keep their current password", fmtInt(s.Bcrypt), fmtInt(s.Total)), Detail: "Supabase stores bcrypt hashes, which PocketBase verifies natively. Users sign in with the same email and password.", Handling: "Automatic."})
		if n := s.Argon2 + s.FbScrypt; n > 0 {
			add(RItem{Severity: "action", Area: "Auth", Title: pl(n, "user has", "users have") + " argon2 / Firebase-scrypt hashes", Detail: "PocketBase only verifies bcrypt. These accounts are imported and flagged `password_reset_required`.", Handling: "Send them a password-reset email after cutover.", Penalty: 3, Effort: 0.5})
		}
		if s.Anonymous > 0 {
			add(RItem{Severity: "action", Area: "Auth", Title: pl(s.Anonymous, "anonymous user is", "anonymous users are") + " skipped", Detail: "PocketBase has no anonymous sign-in.", Handling: "Create guest accounts in app code if you rely on anonymous sessions.", Penalty: 2, Effort: 1})
		}
		if s.PhoneOnly > 0 {
			add(RItem{Severity: "action", Area: "Auth", Title: pl(s.PhoneOnly, "phone-only user", "phone-only users"), Detail: "They are imported, but PocketBase has no SMS OTP login.", Handling: "Add an email to these accounts or build SMS login in a hook.", Penalty: 6, Effort: 4})
		}
		var mfa int64
		for _, n := range s.MFA {
			mfa += n
		}
		if mfa > 0 {
			add(RItem{Severity: "action", Area: "Auth", Title: pl(mfa, "MFA factor", "MFA factors") + " cannot be migrated", Detail: "TOTP secrets are not exportable to PocketBase's MFA model (two auth methods, e.g. password + email OTP).", Handling: "Ask these users to re-enroll; enable PocketBase MFA/OTP.", Penalty: 3, Effort: 1})
		}
		if s.Banned > 0 {
			add(RItem{Severity: "ok", Area: "Auth", Title: pl(s.Banned, "banned user stays", "banned users stay") + " banned", Detail: "`banned_until` becomes a field enforced by the users collection auth rule.", Handling: "Automatic."})
		}
		names := make([]string, 0)
		for p := range s.Providers {
			if p != "email" && p != "phone" {
				names = append(names, p)
			}
		}
		sort.Strings(names)
		unsupported := 0
		resolved := resolveProviders(s.Providers)
		for _, p := range names {
			pb := resolved[p]
			ok := pbProviders[pb]
			if !ok {
				unsupported++
			}
			r.Providers = append(r.Providers, ProviderMap{Supabase: p, PocketBase: pb, Users: s.Providers[p], Supported: ok})
		}
		if len(names) > 0 {
			add(RItem{Severity: "action", Area: "Auth", Title: "Social logins: " + strings.Join(names, ", "), Detail: "Linked accounts are migrated, so users land on the same account. The provider credentials and the redirect URL (`https://<your-app>/api/oauth2-redirect`) must be configured again.", Handling: "Re-enter client id/secret in PocketBase and add the new redirect URL in each provider console.", Penalty: 2, Effort: 0.25 * float64(len(names))})
		}
		if unsupported > 0 {
			add(RItem{Severity: "blocker", Area: "Auth", Title: pl(int64(unsupported), "OAuth provider has", "OAuth providers have") + " no PocketBase equivalent", Detail: "See the provider table below.", Handling: "Use a generic OIDC config if the provider supports OIDC.", Penalty: 15, Effort: 2})
		}
		add(RItem{Severity: "info", Area: "Auth", Title: "Active sessions do not carry over", Detail: "Supabase JWTs are not valid PocketBase tokens. The migration generates a token-exchange endpoint so logged-in users can switch without signing in again while the Supabase project is still running.", Handling: "Call `/api/supabase/exchange` once from the client, or ask users to sign in again."})
	}

	// ---------- security ----------
	for _, t := range a.Tables {
		if !t.RLS {
			add(RItem{Severity: "action", Area: "Security", Title: "RLS is disabled on `" + t.Name + "`", Detail: "On Supabase this table is readable and writable by anyone holding the anon key. It will be migrated locked (superusers only).", Handling: "Decide who may read/write it and set PocketBase API rules.", Penalty: 2, Effort: 0.25})
		}
	}
	if st.Policies > 0 {
		add(RItem{Severity: "ok", Area: "Security", Title: fmt.Sprintf("%d of %d policy rules translated automatically", st.PoliciesAuto, st.Policies), Detail: "Row Level Security policies become PocketBase API rules. Anything not understood is locked instead of opened (fail-closed).", Handling: "Automatic."})
	}
	if st.PoliciesReview > 0 {
		add(RItem{Severity: "action", Area: "Security", Title: pl(int64(st.PoliciesReview), "translated rule needs", "translated rules need") + " a human review", Detail: "Subqueries (EXISTS) and storage folder conventions were mapped to relation filters.", Handling: "Check them in the rules preview below.", Penalty: 1 * st.PoliciesReview, Effort: 0.15 * float64(st.PoliciesReview)})
	}
	if st.PoliciesLocked > 0 {
		add(RItem{Severity: "action", Area: "Security", Title: pl(int64(st.PoliciesLocked), "rule could not be translated and will be", "rules could not be translated and will be") + " locked", Detail: "See the reasons in the rules preview.", Handling: "Write these rules by hand.", Penalty: 2 * st.PoliciesLocked, Effort: 0.5 * float64(st.PoliciesLocked)})
	}

	// ---------- storage ----------
	for _, bk := range a.Buckets {
		st.Objects += bk.Objects
		st.StorageBytes += bk.Bytes
	}
	st.Buckets = len(a.Buckets)
	if st.Buckets > 0 {
		var linked []string
		for _, t := range a.Tables {
			for _, c := range t.Columns {
				if c.LinkBucket != "" {
					linked = append(linked, t.Name+"."+c.Name+" → "+c.LinkBucket)
				}
			}
		}
		d := "Files are copied byte-for-byte. Public buckets stay public; private buckets become protected files."
		if len(linked) > 0 {
			d += " Columns holding storage paths/URLs become file fields: " + strings.Join(linked, ", ") + "."
		}
		// files linked to a table row are served under that table's view rule; the bucket's own SELECT policies no longer apply
		for _, t := range a.Tables {
			for _, c := range t.Columns {
				if c.LinkBucket == "" || bucketPublic(a, c.LinkBucket) {
					continue
				}
				if pols := bucketReadPolicies(a, c.LinkBucket); len(pols) > 0 {
					add(RItem{Severity: "action", Area: "Security", Title: fmt.Sprintf("Files in private bucket `%s` will follow the `%s` view rule", c.LinkBucket, t.Name),
						Detail: fmt.Sprintf("%s.%s points to these files. On Supabase the bucket's Storage policies decide who can download them (%s). In PocketBase anyone who can view the %s record can download its file with a file token, which can be more people.",
							t.Name, c.Name, strings.Join(pols, ", "), t.Name),
						Handling: fmt.Sprintf("Check that this is acceptable, or tighten the %s view rule after migrating.", t.Name), Penalty: 1, Effort: 0.25})
				}
			}
		}
		add(RItem{Severity: "ok", Area: "Storage", Title: fmt.Sprintf("%s in %s (%s)", pl(st.Objects, "file", "files"), pl(int64(st.Buckets), "bucket", "buckets"), fmtBytes(st.StorageBytes)), Detail: d, Handling: "Automatic. Old Supabase file URLs change; a URL map is generated."})
	}

	// ---------- logic ----------
	var rpc []string
	for _, f := range a.Functions {
		if f.Kind == "f" && f.Name != "handle_new_user" {
			isTrig := false
			for _, tg := range a.Triggers {
				if tg.Func == f.Name {
					isTrig = true
				}
			}
			if !isTrig {
				rpc = append(rpc, f.Name+"()")
			}
		}
	}
	st.Functions = len(rpc)
	if len(rpc) > 0 {
		add(RItem{Severity: "action", Area: "Logic", Title: pl(int64(len(rpc)), "database function (RPC)", "database functions (RPC)") + " to rewrite", Detail: strings.Join(rpc, ", ") + " — PostgreSQL functions cannot run in SQLite. A route stub is generated for each.", Handling: "Port each to a pb_hooks route (JavaScript) or Go.", Penalty: 3 * len(rpc), Effort: 1.5 * float64(len(rpc))})
	}
	var trig, touch []string
	for _, tg := range a.Triggers {
		if tg.Touch != "" && tg.Schema == "public" {
			touch = append(touch, fmt.Sprintf("%s on %s (%s)", tg.Name, tg.Table, tg.Touch))
			continue
		}
		if tg.Func == "handle_new_user" {
			add(RItem{Severity: "ok", Area: "Logic", Title: "Signup trigger `" + tg.Name + "` becomes unnecessary", Detail: "It creates a profile row per user; profiles are merged into the PocketBase users collection instead.", Handling: "Automatic."})
			continue
		}
		trig = append(trig, tg.Name+" on "+tg.Table)
	}
	if len(touch) > 0 {
		add(RItem{Severity: "ok", Area: "Logic", Title: pl(int64(len(touch)), "\"updated at\" trigger becomes", "\"updated at\" triggers become") + " an autodate field", Detail: strings.Join(touch, ", ") + ". PocketBase sets the field on every update.", Handling: "Automatic."})
	}
	st.Triggers = len(trig)
	if len(trig) > 0 {
		add(RItem{Severity: "action", Area: "Logic", Title: pl(int64(len(trig)), "trigger", "triggers") + " to rewrite", Detail: strings.Join(trig, ", "), Handling: "Re-implement as onRecordCreate/Update/Delete hooks.", Penalty: 3 * len(trig), Effort: 1 * float64(len(trig))})
	}
	if a.CronJobs > 0 {
		add(RItem{Severity: "action", Area: "Logic", Title: pl(a.CronJobs, "pg_cron job", "pg_cron jobs"), Detail: "Scheduled SQL does not migrate.", Handling: "Re-create them with cronAdd() in pb_hooks.", Penalty: 2, Effort: 0.5 * float64(a.CronJobs)})
	}
	for _, e := range a.Extensions {
		switch e {
		case "timescaledb", "pgroonga", "pgmq":
			add(RItem{Severity: "blocker", Area: "Logic", Title: "Extension " + e + " is installed", Detail: "No SQLite/PocketBase equivalent.", Handling: "Check whether the app actually depends on it.", Penalty: 20, Effort: 8})
		case "pg_net", "http":
			add(RItem{Severity: "info", Area: "Logic", Title: "Extension " + e + " is installed", Detail: "Used by database webhooks / outbound HTTP from SQL.", Handling: "If you use database webhooks, port them to hooks with $http.send()."})
		}
	}
	if len(a.Realtime) > 0 {
		add(RItem{Severity: "info", Area: "Logic", Title: "Realtime enabled on " + strings.Join(a.Realtime, ", "), Detail: "PocketBase has realtime record subscriptions (SSE) that respect API rules. Broadcast and Presence channels are not available.", Handling: "Switch subscriptions to the PocketBase SDK."})
	}
	add(RItem{Severity: "info", Area: "Logic", Title: "Edge Functions are not visible from the database", Detail: "Deno functions live outside Postgres, so this report cannot list them.", Handling: "Port them to pb_hooks, or deploy them as a separate backend service on Pocketbasecloud."})
	add(RItem{Severity: "action", Area: "App", Title: "Client code moves from supabase-js to the PocketBase SDK", Detail: "Queries become `pb.collection(...).getList(page, perPage, {filter, sort, expand})`; auth, storage URLs and realtime calls change too.", Handling: "Budget time per screen/table; a query cheat sheet is included with the migration.", Penalty: 5, Effort: 2 + 0.4*float64(st.Tables)})

	// ---------- behavior differences ----------
	if len(nullCols) > 0 {
		diff(RItem{Area: "Data", Title: "NULL becomes 0, \"\" or false", Detail: fmt.Sprintf("PocketBase fields (except JSON) are never null. %d columns currently contain NULLs: %s.", len(nullCols), short(nullCols, 8)), Handling: "Code that checks `=== null` must check for the empty value instead."})
	}
	if len(nonASCII) > 0 {
		diff(RItem{Area: "Search", Title: "Case-insensitive search only folds ASCII letters", Detail: fmt.Sprintf("SQLite LIKE (PocketBase `~`) treats \"Đức\" and \"đức\" as different. Non-ASCII text found in: %s.", short(nonASCII, 8)), Handling: "Add a lower-cased / unaccented search field maintained by a hook."})
	}
	if a.Collation != "" && !strings.HasPrefix(a.Collation, "C") && a.Collation != "POSIX" {
		diff(RItem{Area: "Sorting", Title: "Text sort order changes", Detail: "Postgres sorts with the " + a.Collation + " collation (Apple, ápple, banana); PocketBase sorts by code point (Apple, Zebra, banana, ápple).", Handling: "Sort in the client for user-facing lists, or store a sort key."})
	}
	if len(textNums) > 0 {
		diff(RItem{Area: "Filters", Title: "Big numbers stored as text compare as text", Detail: "`amount > \"100\"` is a string comparison (\"42\" > \"100\").", Handling: "Filter these fields in app code or keep values within 2^53."})
	}
	if len(tsCols) > 0 {
		diff(RItem{Area: "Dates", Title: "Timestamps keep millisecond precision, in UTC", Detail: "Microseconds are truncated; `date` columns become midnight UTC datetimes (display in local time can show the previous day).", Handling: "Format dates in UTC or store date-only values as text."})
	}
	diff(RItem{Area: "API", Title: "Different query and auth API", Detail: "PostgREST filters (`.eq()`, `.select('*, author(*)')`) map to PocketBase filter strings and `expand`.", Handling: "Covered by the client-code item above."})

	// ---------- score, verdict, effort ----------
	blockers, pen := 0, 0
	for _, it := range r.Items {
		switch it.Severity {
		case "blocker":
			blockers++
		case "action":
			pen += it.Penalty
		}
	}
	score := 100 - int(math.Min(60, float64(25*blockers))) - int(math.Min(45, float64(pen)))
	if blockers > 0 && score > 49 {
		score = 49
	}
	r.Score = score
	switch {
	case blockers > 0:
		r.Verdict, r.VerdictKey = "Blockers found", "red"
		r.VerdictText = "Some features this project uses have no PocketBase equivalent. Review the blockers before deciding."
	case score >= 80:
		r.Verdict, r.VerdictKey = "Good fit", "green"
		r.VerdictText = "Most of the migration is automatic. The remaining work is mainly client code."
	default:
		r.Verdict, r.VerdictKey = "Good fit, with changes", "amber"
		r.VerdictText = "The data, users and files migrate automatically, but plan time for the items below."
	}
	var total float64
	for _, it := range r.Items {
		if it.Effort > 0 {
			r.Effort = append(r.Effort, EffortLine{Label: it.Title, Hours: it.Effort})
			total += it.Effort
		}
	}
	sort.SliceStable(r.Effort, func(i, j int) bool { return r.Effort[i].Hours > r.Effort[j].Hours })
	r.EffortLow, r.EffortHigh = math.Max(1, roundHalf(total*0.7)), math.Max(2, roundHalf(total*1.5))
	r.MigrateSecs = 30 + float64(st.Rows)/20000 + float64(st.Objects)*0.05 + float64(st.StorageBytes)/(20<<20)
	sev := map[string]int{"blocker": 0, "action": 1, "info": 2, "ok": 3}
	sort.SliceStable(r.Items, func(i, j int) bool { return sev[r.Items[i].Severity] < sev[r.Items[j].Severity] })
	return r
}

// pl renders "1 view" / "3 views"
func pl(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmtInt(n) + " " + many
}

func short(v []string, n int) string {
	if len(v) <= n {
		return strings.Join(v, ", ")
	}
	return strings.Join(v[:n], ", ") + fmt.Sprintf(" and %d more", len(v)-n)
}

func fmtInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []string
	for len(s) > 3 {
		out = append([]string{s[len(s)-3:]}, out...)
		s = s[:len(s)-3]
	}
	out = append([]string{s}, out...)
	if neg {
		return "-" + strings.Join(out, ",")
	}
	return strings.Join(out, ",")
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func fmtDuration(secs float64) string {
	if secs < 90 {
		return fmt.Sprintf("~%d s", int(math.Ceil(secs/10)*10))
	}
	if secs < 3600 {
		return fmt.Sprintf("~%d min", int(math.Ceil(secs/60)))
	}
	return fmt.Sprintf("~%.1f h", secs/3600)
}

func bucketPublic(a *Analysis, id string) bool {
	for _, b := range a.Buckets {
		if b.ID == id {
			return b.Public
		}
	}
	return false
}

// bucketReadPolicies lists storage.objects SELECT policies that mention the bucket and do more than allow the whole bucket.
func bucketReadPolicies(a *Analysis, bucket string) []string {
	var out []string
	lit := "bucket_id = '" + bucket + "'"
	for _, p := range a.Policies {
		if p.Schema != "storage" || p.Table != "objects" || (p.Cmd != "SELECT" && p.Cmd != "ALL") {
			continue
		}
		q := stripParens(normalizeExpr(p.Qual))
		if !strings.Contains(q, lit) || q == lit {
			continue
		}
		out = append(out, `"`+p.Name+`"`)
	}
	return out
}
