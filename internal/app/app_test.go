package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
)

func TestParseSource(t *testing.T) {
	cases := []struct {
		dsn, ref, storage, err string
	}{
		{"postgresql://postgres.abcdefghijklmnopqrst:pw@aws-0-eu-central-1.pooler.supabase.com:5432/postgres", "abcdefghijklmnopqrst", "https://abcdefghijklmnopqrst.supabase.co/storage/v1", ""},
		{"postgresql://postgres:pw@db.abcdefghijklmnopqrst.supabase.co:5432/postgres", "abcdefghijklmnopqrst", "https://abcdefghijklmnopqrst.supabase.co/storage/v1", ""},
		{"postgresql://postgres:pw@localhost:54322/postgres", "", "", ""},
		{"postgresql://postgres.abcd:[YOUR-PASSWORD]@aws-0-eu.pooler.supabase.com:5432/postgres", "", "", "[YOUR-PASSWORD]"},
		{"https://abcdefghijklmnopqrst.supabase.co", "", "", "not a Postgres connection string"},
		// transaction pooler: refused with a pointer to the session pooler; port 6543 elsewhere is not judged
		{"postgresql://postgres.abcdefghijklmnopqrst:pw@aws-0-eu-central-1.pooler.supabase.com:6543/postgres", "", "", "Session pooler"},
		{"postgresql://postgres:pw@db.abcdefghijklmnopqrst.supabase.co:6543/postgres", "", "", "Session pooler"},
		{"postgresql://postgres:pw@db.internal.example:6543/postgres", "", "", ""},
		{"", "", "", "empty"},
	}
	for _, c := range cases {
		s, err := ParseSource(c.dsn)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: want error containing %q, got %v", c.dsn, c.err, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", c.dsn, err)
		}
		if s.Ref != c.ref || s.StorageURL() != c.storage {
			t.Errorf("%q: ref=%q storage=%q, want %q %q", c.dsn, s.Ref, s.StorageURL(), c.ref, c.storage)
		}
	}
}

func TestRedactedHidesPassword(t *testing.T) {
	s, err := ParseSource("postgresql://postgres.abcdefghijklmnopqrst:S3cr3t%21pw@aws-0-eu.pooler.supabase.com:5432/postgres")
	if err != nil {
		t.Fatal(err)
	}
	r := s.Redacted()
	if strings.Contains(r, "S3cr3t") || !strings.Contains(r, "****") {
		t.Fatalf("password not redacted: %s", r)
	}
}

func TestSecretKeys(t *testing.T) {
	// header payloads: {"role":"service_role"} and {"role":"anon"}
	service := "eyJhbGciOiJIUzI1NiJ9.eyJyb2xlIjoic2VydmljZV9yb2xlIn0.sig"
	anon := "eyJhbGciOiJIUzI1NiJ9.eyJyb2xlIjoiYW5vbiJ9.sig"
	for key, ok := range map[string]bool{service: true, anon: false, "sb_secret_abc": true, "sb_publishable_abc": false, "hello": false} {
		if err := checkSecretKey(key); (err == nil) != ok {
			t.Errorf("checkSecretKey(%.20s): err=%v, want ok=%v", key, err, ok)
		}
	}
	h := http.Header{}
	setKeyHeaders(h, "sb_secret_abc")
	if h.Get("apikey") != "sb_secret_abc" || h.Get("Authorization") != "" {
		t.Errorf("secret key must only go in apikey: %v", h)
	}
	h = http.Header{}
	setKeyHeaders(h, service)
	if h.Get("Authorization") != "Bearer "+service {
		t.Errorf("legacy JWT key must also be a Bearer token: %v", h)
	}
}

func TestNewTarget(t *testing.T) {
	for in, want := range map[string]string{
		"myapp.pocketbasecloud.com":            "https://myapp.pocketbasecloud.com",
		"https://myapp.pocketbasecloud.com/":   "https://myapp.pocketbasecloud.com",
		"https://myapp.pocketbasecloud.com/_/": "https://myapp.pocketbasecloud.com",
		"http://127.0.0.1:8090":                "http://127.0.0.1:8090",
	} {
		tg, err := NewTarget(in)
		if err != nil || tg.URL != want {
			t.Errorf("NewTarget(%q) = %v, %v; want %q", in, tg, err, want)
		}
	}
	if _, err := NewTarget("ftp://x"); err == nil {
		t.Error("ftp URL accepted")
	}
}

func TestViewToSQLite(t *testing.T) {
	def := ` SELECT p.id,
    p.title,
    (count(c.id))::integer AS comment_count,
    max(c.created_at) AS last_comment_at,
    'draft'::character varying AS status,
    (p.score)::double precision AS score
   FROM (public.posts p
     LEFT JOIN public.comments c ON ((c.post_id = p.id)))
  WHERE (p.title ~~* 'a%'::text) OR p.title ILIKE 'b%'
  GROUP BY p.id;`
	got, err := pgViewToSQLite(def)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CAST((count(c.id)) AS INTEGER)", "CAST('draft' AS TEXT)", "CAST((p.score) AS REAL)", "FROM (posts p", "LIKE 'b%'", "LIKE CAST('a%' AS TEXT)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "::") || strings.Contains(got, "public.") || strings.HasSuffix(got, ";") {
		t.Errorf("leftovers in:\n%s", got)
	}
	if _, err := pgViewToSQLite("SELECT id, tags::text[] FROM posts"); err == nil {
		t.Error("array cast should be reported as manual work")
	}
	if _, err := pgViewToSQLite("SELECT id, x::tsvector FROM posts"); err == nil {
		t.Error("tsvector cast should be reported as manual work")
	}
}

func TestResolveProviders(t *testing.T) {
	got := resolveProviders(map[string]int64{"email": 3, "github": 1, "keycloak": 1, "linkedin_oidc": 2, "azure": 1, "instagram": 1})
	want := map[string]string{"github": "github", "keycloak": "oidc", "linkedin_oidc": "oidc2", "azure": "microsoft", "instagram": "instagram2"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s -> %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["email"]; ok {
		t.Error("email is not an OAuth provider")
	}
}

func TestEmbeddedVersionMatchesConstant(t *testing.T) {
	if v := embeddedPBVersion(); v != pbVersion {
		t.Fatalf("go.mod has PocketBase %s but pbVersion is %s: update the constant", v, pbVersion)
	}
}

func TestCheckVersion(t *testing.T) {
	mk := func(files ...string) string {
		dir := t.TempDir()
		db, err := core.DefaultDBConnect(filepath.Join(dir, "data.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.NewQuery("CREATE TABLE _migrations (file TEXT PRIMARY KEY, applied INTEGER)").Execute()
		for _, f := range files {
			db.Insert("_migrations", map[string]any{"file": f, "applied": 1}).Execute()
		}
		return dir
	}
	var all []string
	for _, m := range core.SystemMigrations.Items() {
		all = append(all, m.File)
	}
	if len(all) == 0 {
		t.Fatal("system migrations are not registered")
	}
	if err := checkVersion(mk(append(all, "1794208786_updated_users.js")...)); err != nil {
		t.Errorf("same release + app migration: %v", err)
	}
	if err := checkVersion(mk(all[:len(all)-1]...)); err == nil || !strings.Contains(err.Error(), "older") {
		t.Errorf("older server not detected: %v", err)
	}
	if err := checkVersion(mk(append(all, "1899999999_future_change.go")...)); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("newer server not detected: %v", err)
	}
}

func TestZipRoundTrip(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "storage", "c1"), 0o755)
	os.WriteFile(filepath.Join(src, "data.db"), []byte("db"), 0o644)
	os.WriteFile(filepath.Join(src, "data.db-wal"), []byte("wal"), 0o644)
	os.WriteFile(filepath.Join(src, "storage", "c1", "f.txt"), []byte("file"), 0o644)
	os.MkdirAll(filepath.Join(src, core.LocalBackupsDirName), 0o755)
	os.WriteFile(filepath.Join(src, core.LocalBackupsDirName, "old.zip"), []byte("x"), 0o644)
	zipPath := filepath.Join(t.TempDir(), "b.zip")
	if err := zipDir(src, zipPath); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := unzip(zipPath, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "storage", "c1", "f.txt")); string(b) != "file" {
		t.Error("file content lost")
	}
	for _, skipped := range []string{"data.db-wal", filepath.Join(core.LocalBackupsDirName, "old.zip")} {
		if _, err := os.Stat(filepath.Join(dst, skipped)); err == nil {
			t.Errorf("%s should not be in the backup", skipped)
		}
	}
}

func TestWriteRateNeedsAStatsWindow(t *testing.T) {
	sev := func(a *Analysis) string {
		for _, it := range buildReadiness(a).Items {
			if strings.Contains(it.Title, "write rate") || strings.Contains(it.Title, "Write rate") {
				return it.Severity
			}
		}
		return ""
	}
	// 150k rows bulk-loaded into a fresh database 3 minutes ago: not a write-heavy app
	if got := sev(&Analysis{Writes: 150000, StatsSecs: 180}); got != "info" {
		t.Errorf("fresh statistics: severity %q, want info", got)
	}
	// 400 writes/s sustained over a week: SQLite single-writer blocker
	if got := sev(&Analysis{Writes: 400 * 7 * 86400, StatsSecs: 7 * 86400}); got != "blocker" {
		t.Errorf("sustained writes: severity %q, want blocker", got)
	}
}

func TestJoinPolicies(t *testing.T) {
	tables := map[string]map[string]string{ // pg column -> pb field
		"memberships": {"org_id": "org_id", "user_id": "user_id", "role": "role"},
		"posts":       {"id": "id", "author_id": "author_id"},
	}
	rc := &ruleCtx{
		table: &Table{Name: "tasks"},
		fieldOf: func(c string) (string, bool) {
			return c, c == "org_id" || c == "id" || c == "created_by" || c == "post_id"
		},
		relTo: func(c string) string {
			if c == "post_id" {
				return "posts"
			}
			return ""
		},
		fieldIn: func(tbl, col string) (string, string, bool) { f, ok := tables[tbl][col]; return tbl, f, ok },
	}
	cases := []struct{ in, want string }{
		{"EXISTS (SELECT 1 FROM memberships m WHERE ((m.org_id = tasks.org_id) AND (m.user_id = auth.uid())))",
			"@collection.memberships:rls_m1.org_id ?= org_id && @collection.memberships:rls_m1.user_id ?= @request.auth.id"},
		{"org_id IN (SELECT memberships.org_id FROM memberships WHERE (memberships.user_id = auth.uid()))",
			"@collection.memberships:rls_memberships2.org_id ?= org_id && @collection.memberships:rls_memberships2.user_id ?= @request.auth.id"},
		{"auth.uid() IN (SELECT m.user_id FROM memberships m WHERE (m.org_id = tasks.org_id))",
			"@collection.memberships:rls_m3.org_id ?= org_id && @collection.memberships:rls_m3.user_id ?= @request.auth.id"},
		{"EXISTS (SELECT 1 FROM memberships m WHERE ((m.user_id = auth.uid()) AND (m.org_id = tasks.org_id) AND (m.role = 'owner')))",
			`@collection.memberships:rls_m4.org_id ?= org_id && @collection.memberships:rls_m4.user_id ?= @request.auth.id && @collection.memberships:rls_m4.role ?= "owner"`},
		{"EXISTS (SELECT 1 FROM posts p WHERE ((p.id = tasks.post_id) AND (p.author_id = auth.uid())))",
			"post_id.author_id = @request.auth.id"},
	}
	for _, c := range cases {
		got, err := rc.translate(c.in)
		if err != nil || got != c.want {
			t.Errorf("%s\n got  %q (%v)\n want %q", c.in, got, err, c.want)
		}
	}
	// a helper function or an unknown table stays untranslatable (rule locked)
	for _, in := range []string{"is_task_member(task_id)", "EXISTS (SELECT 1 FROM teams t WHERE ((t.org_id = tasks.org_id) AND (t.user_id = auth.uid())))"} {
		if got, err := rc.translate(in); err == nil {
			t.Errorf("%s should not translate, got %q", in, got)
		}
	}
	// WITH CHECK on UPDATE only constrains the column when the request changes it, with its own alias
	rc.newRow = true
	got, _ := rc.translate("EXISTS (SELECT 1 FROM memberships m WHERE ((m.org_id = tasks.org_id) AND (m.user_id = auth.uid())))")
	if !strings.HasPrefix(got, "@request.body.org_id:isset = false || (@collection.memberships:rls_m") || strings.Contains(got, "rls_m1.") {
		t.Errorf("newRow rule: %s", got)
	}
}

func TestTouchColumn(t *testing.T) {
	cases := []struct {
		def, fn, src, col string
		ins               bool
	}{
		{"CREATE TRIGGER tasks_updated_at BEFORE UPDATE ON public.tasks FOR EACH ROW EXECUTE FUNCTION extensions.moddatetime('updated_at')", "moddatetime", "", "updated_at", false},
		{"CREATE TRIGGER t BEFORE UPDATE ON public.posts FOR EACH ROW EXECUTE PROCEDURE moddatetime(modified_at)", "moddatetime", "", "modified_at", false},
		{"CREATE TRIGGER t BEFORE INSERT OR UPDATE ON public.posts FOR EACH ROW EXECUTE FUNCTION handle_updated_at()", "handle_updated_at",
			"\nbegin\n  new.updated_at = timezone('utc'::text, now());\n  return new;\nend;\n", "updated_at", true},
		{"CREATE TRIGGER t BEFORE UPDATE ON public.posts FOR EACH ROW EXECUTE FUNCTION set_updated_at()", "set_updated_at",
			"BEGIN NEW.updated_at := now(); RETURN NEW; END", "updated_at", false},
		// does more than touching a column: stays manual work
		{"CREATE TRIGGER t BEFORE UPDATE ON public.posts FOR EACH ROW EXECUTE FUNCTION f()", "f",
			"begin new.updated_at = now(); new.version = old.version + 1; return new; end;", "", false},
		// AFTER trigger: not a touch
		{"CREATE TRIGGER t AFTER UPDATE ON public.posts FOR EACH ROW EXECUTE FUNCTION extensions.moddatetime('updated_at')", "moddatetime", "", "", false},
	}
	for _, c := range cases {
		col, ins := touchColumn(c.def, c.fn, c.src)
		if col != c.col || ins != c.ins {
			t.Errorf("%s\n got %q,%v want %q,%v", c.def, col, ins, c.col, c.ins)
		}
	}
}

func TestInsertRuleWithAuthDefault(t *testing.T) {
	rc := &ruleCtx{table: &Table{Name: "tasks"},
		fieldOf:   func(c string) (string, bool) { return c, true },
		relTo:     func(string) string { return "" },
		meDefault: func(c string) bool { return c == "created_by" }}
	pols := []Policy{
		{Name: "ins", Cmd: "INSERT", Permissive: "PERMISSIVE", Roles: []string{"authenticated"}, Check: "(created_by = ( SELECT auth.uid() AS uid))"},
		{Name: "del", Cmd: "DELETE", Permissive: "PERMISSIVE", Roles: []string{"authenticated"}, Qual: "(created_by = ( SELECT auth.uid() AS uid))"},
	}
	r := buildRules(rc, true, pols)
	if got, want := r["INSERT"].Rule, `@request.auth.id != "" && ((created_by = "" || created_by = @request.auth.id))`; got != want {
		t.Errorf("insert rule\n got  %s\n want %s", got, want)
	}
	if got, want := r["DELETE"].Rule, `@request.auth.id != "" && (@request.auth.id = created_by)`; got != want {
		t.Errorf("delete rule must stay strict\n got  %s\n want %s", got, want)
	}
}

func TestBucketReadPolicies(t *testing.T) {
	a := &Analysis{Policies: []Policy{
		{Schema: "storage", Table: "objects", Name: "public avatars", Cmd: "SELECT", Qual: "(bucket_id = 'avatars'::text)"},
		{Schema: "storage", Table: "objects", Name: "own files", Cmd: "SELECT", Qual: "((bucket_id = 'attachments'::text) AND ((storage.foldername(name))[1] = (( SELECT auth.uid() AS uid))::text))"},
		{Schema: "storage", Table: "objects", Name: "upload", Cmd: "INSERT", Check: "(bucket_id = 'attachments'::text)"},
	}}
	if got := bucketReadPolicies(a, "attachments"); len(got) != 1 || got[0] != `"own files"` {
		t.Errorf("attachments: %v", got)
	}
	if got := bucketReadPolicies(a, "avatars"); len(got) != 0 {
		t.Errorf("avatars (whole bucket readable) should not warn: %v", got)
	}
}

// A policy only opens a rule for the app roles it names and that hold the table privilege. Policies for
// service_role, internal roles or custom roles never gave app users access and must leave the rule locked.
func TestRulesFollowPolicyRolesAndGrants(t *testing.T) {
	newRC := func(privs map[string]map[string]bool) *ruleCtx {
		return &ruleCtx{table: &Table{Name: "t", Privs: privs},
			fieldOf: func(c string) (string, bool) { return c, true }, relTo: func(string) string { return "" }}
	}
	all := map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true}
	pol := func(cmd string, qual string, roles ...string) Policy {
		return Policy{Name: cmd + " " + strings.Join(roles, "+"), Cmd: cmd, Permissive: "PERMISSIVE", Roles: roles, Qual: qual, Check: qual}
	}
	cases := []struct {
		name   string
		privs  map[string]map[string]bool
		pols   []Policy
		cmd    string
		status string
		rule   string
	}{
		{"service_role only stays locked", nil, []Policy{pol("ALL", "true", "service_role")}, "SELECT", "locked", ""},
		{"service_role only stays locked for writes", nil, []Policy{pol("ALL", "true", "service_role")}, "DELETE", "locked", ""},
		{"internal role stays locked", nil, []Policy{pol("SELECT", "true", "supabase_auth_admin")}, "SELECT", "locked", ""},
		{"custom role stays locked", nil, []Policy{pol("SELECT", "true", "reporting")}, "SELECT", "locked", ""},
		{"public is everyone", nil, []Policy{pol("SELECT", "true", "public")}, "SELECT", "translated", ""},
		{"anon and authenticated is everyone", nil, []Policy{pol("SELECT", "true", "anon", "authenticated")}, "SELECT", "translated", ""},
		{"authenticated with service_role is signed-in users only", nil, []Policy{pol("SELECT", "true", "authenticated", "service_role")}, "SELECT", "translated", `@request.auth.id != ""`},
		{"anon only", nil, []Policy{pol("SELECT", "true", "anon")}, "SELECT", "translated", `@request.auth.id = ""`},
		{"service_role policy does not widen a user policy", nil,
			[]Policy{pol("SELECT", "true", "service_role"), pol("SELECT", "(user_id = auth.uid())", "authenticated")},
			"SELECT", "translated", `@request.auth.id != "" && (@request.auth.id = user_id)`},
		{"no privilege for either role", map[string]map[string]bool{"anon": {}, "authenticated": {}}, []Policy{pol("SELECT", "true", "public")}, "SELECT", "locked", ""},
		{"privilege only for authenticated", map[string]map[string]bool{"anon": {}, "authenticated": all}, []Policy{pol("SELECT", "true", "public")}, "SELECT", "translated", `@request.auth.id != ""`},
		{"privilege only for anon, policy only for authenticated", map[string]map[string]bool{"anon": all, "authenticated": {}}, []Policy{pol("SELECT", "true", "authenticated")}, "SELECT", "locked", ""},
		{"read privilege does not open writes", map[string]map[string]bool{"anon": {"SELECT": true}, "authenticated": {"SELECT": true}}, []Policy{pol("ALL", "true", "public")}, "INSERT", "locked", ""},
	}
	for _, c := range cases {
		r := buildRules(newRC(c.privs), true, c.pols)[c.cmd]
		if r.Status != c.status || r.Rule != c.rule {
			t.Errorf("%s: got %s %q (%s), want %s %q", c.name, r.Status, r.Rule, r.Err, c.status, c.rule)
		}
		if r.Status == "locked" && r.Err == "" {
			t.Errorf("%s: a locked rule must say why", c.name)
		}
	}
	// the report names the policies that were left out
	r := buildRules(newRC(nil), true, []Policy{pol("SELECT", "true", "service_role")})["SELECT"]
	if !strings.Contains(r.Err, "service_role") || len(r.Sources) != 0 {
		t.Errorf("skipped policy should be explained and not listed as a source: %+v", r)
	}
}

func userKeyed(name, pk string, cols ...string) *Table {
	t := &Table{Name: name, PK: []string{pk}, FKs: []FK{{Cols: []string{pk}, RefTable: "auth.users", RefCols: []string{"id"}}},
		Columns: []*Column{{Name: pk, TypName: "uuid", Type: "uuid"}}}
	for _, c := range cols {
		typ := "text"
		if c == "verified" {
			typ = "bool"
		}
		t.Columns = append(t.Columns, &Column{Name: c, TypName: typ, Type: typ})
	}
	return t
}

// Only one table keyed by the user id is merged into users; the others stay collections, so no table is dropped.
func TestOneProfileTableIsMerged(t *testing.T) {
	plan := func(tables ...*Table) *Builder {
		b := &Builder{a: &Analysis{Tables: tables}}
		b.plan()
		return b
	}
	merged := func(b *Builder) string {
		if b.users == nil {
			return ""
		}
		return b.users.PGName
	}
	posts := &Table{Name: "posts", PK: []string{"id"}, Columns: []*Column{{Name: "id", TypName: "uuid", Type: "uuid"}}}

	if got := merged(plan(userKeyed("user_settings", "user_id", "theme"), posts)); got != "user_settings" {
		t.Errorf("a single per-user table is merged whatever its name, got %q", got)
	}
	b := plan(userKeyed("user_prefs", "id", "theme"), userKeyed("profiles", "id", "username"), posts)
	if got := merged(b); got != "profiles" {
		t.Errorf("profiles should be the merged table, got %q", got)
	}
	prefs := b.byPG["user_prefs"]
	if prefs.Users || prefs.Name != "user_prefs" || prefs.IdMode != "uuid" || prefs.field("id").Kind != "id" {
		t.Errorf("user_prefs must stay its own collection keyed by the user id: %+v", prefs)
	}
	if prefs.field("theme") == nil || b.byPG["profiles"].field("username") == nil {
		t.Error("columns of both tables must be planned")
	}
	if got := merged(plan(userKeyed("user_settings", "user_id", "theme"), userKeyed("user_stats", "user_id", "logins"))); got != "" {
		t.Errorf("no profile-like name among several candidates: none is merged, got %q", got)
	}
	if got := merged(plan(posts)); got != "" {
		t.Errorf("no per-user table, got %q", got)
	}
}

// Profile columns must not overwrite what users gets from auth.users (email, verified, password, phone...).
func TestMergedProfileColumnsKeepAuthFields(t *testing.T) {
	b := &Builder{a: &Analysis{Tables: []*Table{userKeyed("profiles", "user_id", "email", "verified", "password", "phone", "name", "full_name", "avatar", "username", "profile_phone")}}}
	b.plan()
	want := map[string]string{"email": "profile_email", "verified": "profile_verified", "password": "profile_password", "phone": "profile_phone_",
		"name": "name", "full_name": "full_name", "avatar": "profile_avatar", "username": "username", "profile_phone": "profile_phone"}
	seen := map[string]string{}
	for pg, pb := range want {
		fp := b.users.field(pg)
		if fp == nil || fp.Name != pb {
			t.Errorf("profiles.%s -> %+v, want field %q", pg, fp, pb)
			continue
		}
		if other, dup := seen[strings.ToLower(fp.Name)]; dup {
			t.Errorf("profiles.%s and profiles.%s both map to %q", pg, other, fp.Name)
		}
		seen[strings.ToLower(fp.Name)] = pg
	}
	// without a name column, full_name takes the built-in users.name field; reserved names match in any case
	b = &Builder{a: &Analysis{Tables: []*Table{userKeyed("profiles", "id", "full_name", "Email", "tokenKey")}}}
	b.plan()
	for pg, pb := range map[string]string{"full_name": "name", "Email": "profile_Email", "tokenKey": "profile_tokenKey"} {
		if got := b.users.field(pg).Name; got != pb {
			t.Errorf("%s -> %q, want %q", pg, got, pb)
		}
	}
}

// A download that is cut off, shorter than announced or failing must be an error, never a (partial) file.
func TestFetchObjectRejectsPartialDownloads(t *testing.T) {
	old := fetchBackoff
	fetchBackoff = time.Millisecond
	defer func() { fetchBackoff = old }()
	body := strings.Repeat("x", 4096)
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
	cut := func(w http.ResponseWriter) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte(body[:1000]))
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close() // connection drops mid-file
		}
	}
	mux.HandleFunc("/cut", func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); cut(w) })
	mux.HandleFunc("/flaky", func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) < 3 {
			cut(w)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/busy", func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/stall", func(w http.ResponseWriter, r *http.Request) { // sends a part, then nothing more
		hits.Add(1)
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte(body[:1000]))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) { // 8 chunks, 40 ms apart: 320 ms in total
		hits.Add(1)
		for i := 0; i < 8; i++ {
			_, _ = w.Write([]byte(body[i*512 : (i+1)*512]))
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := &http.Client{Timeout: 5 * time.Second}

	if data, err := fetchObject(client, srv.URL+"/ok", ""); err != nil || string(data) != body {
		t.Fatalf("complete download: %d bytes, %v", len(data), err)
	}
	hits.Store(0)
	if data, err := fetchObject(client, srv.URL+"/cut", ""); err == nil || data != nil {
		t.Errorf("cut download returned %d bytes and no error", len(data))
	} else if hits.Load() != 3 {
		t.Errorf("cut download tried %d times, want 3", hits.Load())
	}
	hits.Store(0)
	if data, err := fetchObject(client, srv.URL+"/flaky", ""); err != nil || string(data) != body {
		t.Errorf("a download that succeeds on the third try: %d bytes, %v", len(data), err)
	}
	hits.Store(0)
	if data, err := fetchObject(client, srv.URL+"/busy", ""); err != nil || string(data) != body {
		t.Errorf("HTTP 503 then 200: %d bytes, %v", len(data), err)
	}
	hits.Store(0)
	if _, err := fetchObject(client, srv.URL+"/missing", ""); err == nil || !strings.Contains(err.Error(), "404") || hits.Load() != 1 {
		t.Errorf("HTTP 404 is final: %v after %d tries", err, hits.Load())
	}
	// no bytes for stallTimeout: given up and retried, whatever the size of the file or the client's own limit
	oldStall := stallTimeout
	stallTimeout = 100 * time.Millisecond
	hits.Store(0)
	if data, err := fetchObject(&http.Client{}, srv.URL+"/stall", ""); err == nil || data != nil || !strings.Contains(err.Error(), "stalled") || hits.Load() != 3 {
		t.Errorf("a stalled download must be given up and retried: %d bytes, %v, %d tries", len(data), err, hits.Load())
	}
	// a slow download that keeps sending is not a stall
	hits.Store(0)
	if data, err := fetchObject(&http.Client{}, srv.URL+"/slow", ""); err != nil || string(data) != body || hits.Load() != 1 {
		t.Errorf("a slow but steady download must succeed: %d bytes, %v, %d tries", len(data), err, hits.Load())
	}
	stallTimeout = oldStall
	// the client's own timeout is final: trying again would only wait as long
	hits.Store(0)
	impatient := &http.Client{Timeout: 200 * time.Millisecond}
	if data, err := fetchObject(impatient, srv.URL+"/stall", ""); err == nil || data != nil || hits.Load() != 1 {
		t.Errorf("a client timeout must fail without a retry: %d bytes, %v, %d tries", len(data), err, hits.Load())
	}
}

// Schemas PocketBase cannot hold are reported by the analysis and stop migrate before the target is touched.
func TestSchemaStoppers(t *testing.T) {
	col := func(names ...string) []*Column {
		out := []*Column{{Name: "id", TypName: "uuid", Type: "uuid"}}
		for _, n := range names {
			out = append(out, &Column{Name: n, TypName: "text", Type: "text"})
		}
		return out
	}
	tbl := func(name string, cols ...string) *Table {
		return &Table{Name: name, PK: []string{"id"}, Columns: col(cols...)}
	}
	titles := func(tables ...*Table) string {
		r := buildReadiness(&Analysis{Tables: tables})
		var out []string
		for _, it := range r.Stoppers() {
			if it.Severity != "blocker" || it.Handling == "" {
				t.Errorf("a stopper must be a blocker with a way out: %+v", it)
			}
			out = append(out, it.Title)
		}
		return strings.Join(out, " | ")
	}
	if got := titles(tbl("posts", "title"), tbl("CamelCase", "userId"), tbl("_internal", "v"), tbl("2fa_codes", "code")); got != "" {
		t.Errorf("valid names must not stop a migration: %s", got)
	}
	for _, c := range []struct {
		tables []*Table
		want   []string
	}{
		{[]*Table{tbl("Order Items", "price")}, []string{"Table names", "`Order Items`"}},
		{[]*Table{tbl("khách_hàng", "ten")}, []string{"Table names", "`khách_hàng`"}},
		{[]*Table{tbl("my-table", "v")}, []string{"Table names", "`my-table`"}},
		{[]*Table{tbl("orders", "Unit Price", "my-col", "tên")}, []string{"Column names", "`orders.Unit Price`", "`orders.my-col`", "`orders.tên`"}},
		{[]*Table{tbl("orders", "Email", "email")}, []string{"Column names", "once case is ignored"}},
		{[]*Table{tbl("users", "nickname")}, []string{"built-in PocketBase collection", "`users`"}},
		{[]*Table{tbl("Posts", "a"), tbl("posts", "b")}, []string{"differ only in upper/lower case", "`Posts` / `posts`"}},
	} {
		got := titles(c.tables...)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: stoppers %q should mention %q", c.tables[0].Name, got, w)
			}
		}
	}
	// a users table keyed by the auth user id is the profile table, not a clash
	if got := titles(userKeyed("users", "id", "nickname")); got != "" {
		t.Errorf("public.users keyed by auth.users is merged, not refused: %s", got)
	}
}

// Columns named like a reserved PocketBase field are kept under another name, and rows of a table without a
// primary key get distinct ids even when two rows are identical.
func TestReservedColumnsAndRowsWithoutKey(t *testing.T) {
	text := func(names ...string) []*Column {
		var out []*Column
		for _, n := range names {
			out = append(out, &Column{Name: n, TypName: "text", Type: "text"})
		}
		return out
	}
	b := &Builder{a: &Analysis{Tables: []*Table{
		{Name: "imported", Columns: text("id", "expand", "collectionId", "name", "id_col")}, // no primary key
		{Name: "pairs", PK: []string{"id", "lang"}, Columns: text("id", "lang", "v")},       // id is only part of the key
		{Name: "docs", PK: []string{"code"}, Columns: text("code", "ID", "title")},          // another column is the key
	}}}
	b.plan()
	for ref, want := range map[string]string{"imported.id": "id_col_", "imported.expand": "expand_col", "imported.collectionId": "collectionId_col",
		"imported.name": "name", "pairs.id": "id_col", "pairs.lang": "lang", "docs.ID": "ID_col", "docs.title": "title"} {
		tbl, col, _ := strings.Cut(ref, ".")
		if fp := b.byPG[tbl].field(col); fp == nil || fp.Name != want {
			t.Errorf("%s -> %+v, want field %q", ref, fp, want)
		}
	}
	if got := buildReadiness(b.a).Stoppers(); len(got) != 0 {
		t.Errorf("renamed columns must not stop the migration: %+v", got)
	}
	noKey, pairs := b.byPG["imported"], b.byPG["pairs"]
	row := map[string]json.RawMessage{"id": json.RawMessage(`"7"`), "expand": json.RawMessage(`"x"`), "lang": json.RawMessage(`"vi"`)}
	if a, c := recordID(noKey, row, 1), recordID(noKey, row, 2); a == c || len(a) != 15 {
		t.Errorf("identical rows without a key must get different ids: %q %q", a, c)
	}
	if a, c := recordID(noKey, row, 5), recordID(noKey, row, 5); a != c {
		t.Errorf("the same row at the same position must keep its id: %q %q", a, c)
	}
	if a, c := recordID(pairs, row, 1), recordID(pairs, row, 2); a != c {
		t.Errorf("with a primary key the id depends on the key only: %q %q", a, c)
	}
}
