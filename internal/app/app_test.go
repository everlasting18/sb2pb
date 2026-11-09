package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
