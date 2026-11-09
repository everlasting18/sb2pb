package app

// sb2pb migrate: connection string in, PocketBase instance out.
//
//   1. analyze the source (read-only) and show what will happen
//   2. back up the target and check its data format matches the embedded PocketBase release
//   3. re-read the source inside one REPEATABLE READ READ ONLY transaction (consistent snapshot)
//   4. build the new pb_data offline on top of the target's backup (keeps superusers, settings, OAuth config)
//   5. upload it as a backup, restore it, wait for the server and verify record counts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
)

type migrateOpts struct {
	from, to, email, storage, hooksOut, report, work     string
	yes, allowBlockers, skipFiles, keepWork, skipVersion bool
}

func runMigrate(ui *UI, args []string) error {
	fs := newFlags("migrate", "Migrate a Supabase project (schema, users, data, files, access rules) into a PocketBase instance.")
	o := migrateOpts{}
	fs.StringVar(&o.from, "from", "", "Supabase connection string (default: $SUPABASE_DB_URL, or prompt)")
	fs.StringVar(&o.to, "to", "", "PocketBase URL, e.g. https://myapp.pocketbasecloud.com (default: $PB_URL, or prompt)")
	fs.StringVar(&o.email, "email", "", "PocketBase superuser email (default: $PB_SUPERUSER_EMAIL, or prompt)")
	fs.StringVar(&o.storage, "storage", "", "Supabase Storage URL; derived from the project ref on hosted Supabase (self-hosted: http://<host>:8000/storage/v1)")
	fs.BoolVar(&o.skipFiles, "skip-files", false, "do not copy Storage files (records keep the file names, content is missing)")
	fs.StringVar(&o.hooksOut, "hooks-out", "pb_hooks", "directory for the generated PocketBase hooks")
	fs.StringVar(&o.report, "report", "sb2pb-report.json", "migration report file")
	fs.StringVar(&o.work, "work", "", "working directory (default: a private temp dir, deleted afterwards)")
	fs.BoolVar(&o.keepWork, "keep-work", false, "keep the working directory (it contains a full copy of the migrated data)")
	fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation (for scripts and CI)")
	fs.BoolVar(&o.allowBlockers, "allow-blockers", false, "migrate even if the readiness check finds blockers")
	fs.BoolVar(&o.skipVersion, "skip-version-check", false, "advanced: do not require the server to run PocketBase "+pbVersion)
	if args != nil {
		if err := parseFlags(fs, args); err != nil {
			return err
		}
	}
	ctx := context.Background()
	started := time.Now()
	ui.Title("sb2pb migrate · Supabase → " + pocketbaseTarget)
	if err := checkOutputs(o.hooksOut, o.report); err != nil { // fail now, not after the data has been built
		return err
	}

	// ---------- source ----------
	src, conn, err := connectSource(ui, o.from, analyzeSession)
	if err != nil {
		return err
	}
	a, err := analyze(ctx, conn, AnalyzeOptions{SampleRows: 50000})
	conn.Close(ctx)
	if err != nil {
		return fmt.Errorf("analysis failed: %s", friendly(err))
	}
	rep := readinessFor(src, a, started)
	printSummary(ui, rep)
	printAttention(ui, rep, 6)
	if !a.HasAuth {
		ui.Warn("No Supabase auth schema found: is this a Supabase database?")
	}
	if n := rep.Count("blocker"); n > 0 && !o.allowBlockers {
		if !ui.interactive || o.yes {
			return fmt.Errorf("%d blocker(s) found; fix them first or pass --allow-blockers to migrate anyway", n)
		}
		ok, err := ui.Confirm("Blockers found: those parts will not work after the move. Migrate anyway?", false)
		if err != nil || !ok {
			return errCancelled
		}
	}

	// ---------- storage access ----------
	storageURL, secretKey := "", ""
	if rep.Stats.Objects > 0 && !o.skipFiles {
		ui.Section("Supabase Storage")
		storageURL, secretKey, err = storageAccess(ui, src, o.storage, rep.Stats.Objects, rep.Stats.Buckets)
		if err != nil {
			return err
		}
	}

	// ---------- target ----------
	ui.Section("PocketBase")
	tg, email, password, err := connectTarget(ui, o.to, o.email, "The PocketBase instance to migrate into, e.g. https://myapp.pocketbasecloud.com")
	if err != nil {
		return err
	}
	if err := tg.CheckEmpty(); err != nil {
		return err
	}
	ui.OK("Target is empty and ready")

	// ---------- confirm ----------
	ui.Section("Ready")
	ui.Info("All data on %s will be replaced by the migrated project.", ui.bold(tg.URL))
	ui.Info("The current state is backed up first; undo with: sb2pb rollback --to %s", tg.URL)
	if !o.yes {
		if !ui.interactive {
			return errors.New("not running in a terminal: re-run with --yes to confirm")
		}
		ok, err := ui.Confirm("Start the migration?", false)
		if err != nil || !ok {
			return errCancelled
		}
	}

	// ---------- run ----------
	work := o.work
	if work == "" {
		if work, err = os.MkdirTemp("", "sb2pb-"); err != nil {
			return err
		}
	} else if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	if !o.keepWork {
		defer os.RemoveAll(work) // holds a full copy of the customer's data
	}
	res, err := execute(ctx, ui, &runCtx{opts: o, src: src, tg: tg, email: email, password: password, storageURL: storageURL,
		secretKey: secretKey, work: work, readiness: rep, started: started})
	if res != nil {
		writeReport(ui, o.report, res)
	}
	if err != nil {
		if res != nil && res.Backup != "" && !res.Restored {
			return fmt.Errorf("%w\n\nThe PocketBase data was not modified (only the safety backup %s was created). Fix the cause and run the migration again", err, res.Backup)
		}
		return err
	}
	nextSteps(ui, res, src, tg, o.report)
	return nil
}

// checkOutputs makes sure the hooks folder and the report can be written.
func checkOutputs(hooksDir, report string) error {
	hint := ""
	if _, err := os.Stat("/.dockerenv"); err == nil || os.Getuid() == 65532 {
		hint = ` (Docker: run the image with --user "$(id -u):$(id -g)" so the mounted folder is writable)`
	}
	for _, dir := range []string{hooksDir, filepath.Dir(report)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("cannot write to %s: %w%s", dir, err, hint)
		}
		f, err := os.CreateTemp(dir, ".sb2pb-write-test-")
		if err != nil {
			return fmt.Errorf("cannot write to %s: %w%s", dir, err, hint)
		}
		f.Close()
		os.Remove(f.Name())
	}
	return nil
}

func storageAccess(ui *UI, src *Source, flagURL string, objects int64, buckets int) (string, string, error) {
	storageURL := flagURL
	if storageURL == "" {
		storageURL = os.Getenv("SUPABASE_STORAGE_URL")
	}
	if storageURL == "" {
		storageURL = src.StorageURL()
	}
	if storageURL == "" {
		v, err := ui.Ask("Storage URL", "Not a hosted Supabase project: enter the Storage API URL (self-hosted: http://<host>:8000/storage/v1)", "")
		if errors.Is(err, errNoInput) {
			return "", "", errors.New("cannot derive the Storage URL: pass --storage, or --skip-files to migrate without file content")
		}
		if err != nil {
			return "", "", err
		}
		storageURL = v
	}
	storageURL = strings.TrimRight(storageURL, "/")
	for attempt := 1; ; attempt++ {
		key, err := ui.value("", []string{"SUPABASE_SECRET_KEY", "SUPABASE_SERVICE_ROLE_KEY"}, func() (string, error) {
			return ui.Secret("Secret key", fmt.Sprintf("Needed to copy %s files: Dashboard → Project Settings → API Keys → secret key (sb_secret_...) or legacy service_role", fmtInt(objects)))
		})
		if errors.Is(err, errNoInput) {
			return "", "", errors.New("Storage has files: set SUPABASE_SECRET_KEY, or pass --skip-files to migrate without file content")
		}
		if err != nil {
			return "", "", err
		}
		if err = checkSecretKey(key); err == nil {
			err = probeStorage(storageURL, key, buckets)
		}
		if err == nil {
			ui.OK("Storage reachable at %s", storageURL)
			return storageURL, key, nil
		}
		if !ui.interactive || attempt >= 3 || os.Getenv("SUPABASE_SECRET_KEY") != "" || os.Getenv("SUPABASE_SERVICE_ROLE_KEY") != "" {
			return "", "", err
		}
		ui.Warn("%s", err)
	}
}

// probeStorage lists buckets: a secret/service_role key sees all of them, other keys only public ones.
func probeStorage(base, key string, wantBuckets int) error {
	req, err := http.NewRequest("GET", base+"/bucket", nil)
	if err != nil {
		return fmt.Errorf("invalid Storage URL: %w", err)
	}
	setKeyHeaders(req.Header, key)
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach Supabase Storage at %s: %w", base, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 400:
		return fmt.Errorf("Supabase Storage rejected the key (HTTP %d): use the secret key (sb_secret_...) or the legacy service_role key", resp.StatusCode)
	case resp.StatusCode != 200:
		return fmt.Errorf("unexpected answer from Supabase Storage (HTTP %d) at %s", resp.StatusCode, base)
	}
	var list []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&list); err != nil {
		return fmt.Errorf("%s did not answer like the Supabase Storage API", base)
	}
	if len(list) < wantBuckets {
		return fmt.Errorf("this key sees %d of %d buckets: use the secret key (sb_secret_...) or the legacy service_role key", len(list), wantBuckets)
	}
	return nil
}

func connectTarget(ui *UI, flagURL, flagEmail, hint string) (*Target, string, string, error) {
	raw, err := ui.value(flagURL, []string{"PB_URL"}, func() (string, error) {
		return ui.Ask("URL", hint, "")
	})
	if errors.Is(err, errNoInput) {
		return nil, "", "", errors.New("no PocketBase URL: pass --to or set PB_URL")
	}
	if err != nil {
		return nil, "", "", err
	}
	tg, err := NewTarget(raw)
	if err != nil {
		return nil, "", "", err
	}
	if err := tg.Health(); err != nil {
		return nil, "", "", err
	}
	email, err := ui.value(flagEmail, []string{"PB_SUPERUSER_EMAIL"}, func() (string, error) {
		return ui.Ask("Superuser email", "", "")
	})
	if errors.Is(err, errNoInput) {
		return nil, "", "", errors.New("no superuser email: pass --email or set PB_SUPERUSER_EMAIL")
	}
	if err != nil {
		return nil, "", "", err
	}
	envPw := os.Getenv("PB_SUPERUSER_PASSWORD")
	for attempt := 1; ; attempt++ {
		password, err := ui.value("", []string{"PB_SUPERUSER_PASSWORD"}, func() (string, error) {
			return ui.Secret("Superuser password", "")
		})
		if errors.Is(err, errNoInput) {
			return nil, "", "", errors.New("no superuser password: set PB_SUPERUSER_PASSWORD")
		}
		if err != nil {
			return nil, "", "", err
		}
		err = tg.Login(email, password)
		if err == nil {
			ui.OK("Signed in to %s as %s", tg.URL, email)
			return tg, email, password, nil
		}
		if envPw != "" || !ui.interactive || attempt >= 3 {
			return nil, "", "", err
		}
		ui.Warn("%s", err)
	}
}

// ---------- execution ----------

type runCtx struct {
	opts                        migrateOpts
	src                         *Source
	tg                          *Target
	email, password             string
	storageURL, secretKey, work string
	readiness                   *Readiness
	started                     time.Time
}

type migrationResult struct {
	Tool       string            `json:"tool"`
	PocketBase string            `json:"pocketbase"`
	Source     map[string]string `json:"source"`
	Target     string            `json:"target"`
	StartedAt  time.Time         `json:"startedAt"`
	FinishedAt time.Time         `json:"finishedAt,omitempty"`
	Backup     string            `json:"preMigrationBackup,omitempty"`
	Uploaded   string            `json:"uploadedBackup,omitempty"`
	Restored   bool              `json:"restored"`
	Verified   map[string][2]int `json:"verifiedCounts,omitempty"` // collection -> [imported, on server]
	Error      string            `json:"error,omitempty"`
	Readiness  map[string]any    `json:"readiness"`
	Migration  *Report           `json:"migration,omitempty"`
	HooksFile  string            `json:"hooksFile,omitempty"`
	OAuth      []ProviderMap     `json:"oauthProviders,omitempty"`
}

func execute(ctx context.Context, ui *UI, rc *runCtx) (res *migrationResult, err error) {
	const steps = 6
	tg := rc.tg
	res = &migrationResult{Tool: "sb2pb " + toolVersion, PocketBase: pbVersion, Target: tg.URL, StartedAt: rc.started,
		Source:    map[string]string{"host": rc.src.Host, "project": rc.src.Ref, "database": rc.src.Database},
		Readiness: map[string]any{"score": rc.readiness.Score, "verdict": rc.readiness.Verdict, "blockers": rc.readiness.Count("blocker"), "actions": rc.readiness.Count("action")},
		OAuth:     rc.readiness.Providers}
	defer func() {
		res.FinishedAt = time.Now().UTC()
		if err != nil {
			res.Error = err.Error()
		}
	}()
	timings := map[string]string{}
	since := func(k string, t time.Time) { timings[k] = time.Since(t).Round(time.Millisecond).String() }

	// 1. safety backup of the target = baseline we build on (keeps superusers, settings, OAuth config)
	ui.Step(1, steps, "Backing up the PocketBase instance")
	t := time.Now()
	stamp := time.Now().UTC().Format("20060102_150405")
	res.Backup = "pre_migration_" + stamp + ".zip"
	if err := tg.CreateBackup(res.Backup); err != nil {
		res.Backup = ""
		return res, fmt.Errorf("could not create the safety backup: %w", err)
	}
	baseZip := filepath.Join(rc.work, res.Backup)
	if err := tg.DownloadBackup(res.Backup, baseZip); err != nil {
		return res, fmt.Errorf("could not download the safety backup: %w", err)
	}
	dataDir := filepath.Join(rc.work, "pb_data")
	if err := unzip(baseZip, dataDir); err != nil {
		return res, err
	}
	os.Remove(baseZip)
	if rc.opts.skipVersion {
		ui.Warn("Version check skipped")
	} else if err := checkVersion(dataDir); err != nil {
		return res, err
	}
	ui.OK("Backup %s created on the server · data format matches PocketBase %s", res.Backup, pbVersion)
	since("backup", t)

	// 2. consistent snapshot of the source
	ui.Step(2, steps, "Reading Supabase (read-only, consistent snapshot)")
	t = time.Now()
	db, err := rc.src.Connect(ctx, migrateSession)
	if err != nil {
		return res, err
	}
	defer db.Close(context.Background())
	if _, err := db.Exec(ctx, "begin isolation level repeatable read read only"); err != nil {
		return res, errors.New(friendly(err))
	}
	a, err := analyze(ctx, db, AnalyzeOptions{}) // full profile: text/JSON size limits come from real maxima
	if err != nil {
		return res, fmt.Errorf("analysis failed: %s", friendly(err))
	}
	ui.OK("%d tables, %s rows, %s users, %s files", len(a.Tables), fmtInt(sumRows(a)), fmtInt(a.Auth.Total), fmtInt(countObjects(a)))
	since("analyze", t)

	// 3. build pb_data offline with the same PocketBase core
	ui.Step(3, steps, "Building the PocketBase data")
	t = time.Now()
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: dataDir})
	if err := app.Bootstrap(); err != nil {
		return res, fmt.Errorf("cannot open the backup with PocketBase %s: %w", pbVersion, err)
	}
	mrep := &Report{Timings: timings, Files: map[string]any{}}
	res.Migration = mrep
	b := &Builder{ctx: ctx, db: db, app: app, a: a, storageURL: rc.storageURL, serviceKey: rc.secretKey, skipFiles: rc.storageURL == "",
		linked: map[string]bool{}, rep: mrep, warnings: map[string]int{}, provider: resolveProviders(a.Auth.Providers),
		http: &http.Client{Timeout: 10 * time.Minute}}
	b.plan()
	steps3 := []struct {
		name string
		fn   func() error
	}{{"collections", b.createCollections}, {"users", b.importUsers}, {"tables", b.importTables}, {"storage objects", b.importOrphanObjects}}
	for _, s := range steps3 {
		if err := s.fn(); err != nil {
			return res, fmt.Errorf("building %s: %w", s.name, err)
		}
	}
	since("build", t)

	ui.Step(4, steps, "Copying files, then indexes, rules and views")
	t = time.Now()
	if err := b.copyFiles(); err != nil {
		return res, fmt.Errorf("copying files: %w", err)
	}
	if err := b.indexesAndRules(); err != nil {
		return res, err
	}
	b.views()
	if err := b.hooks(rc.opts.hooksOut, rc.src.AuthURL()); err != nil {
		return res, fmt.Errorf("writing hooks: %w", err)
	}
	res.HooksFile = filepath.Join(rc.opts.hooksOut, "supabase_migration.pb.js")
	mrep.Plans, mrep.Warnings = b.plans, b.warnings
	_, _ = db.Exec(ctx, "commit") // read-only transaction: nothing to commit
	mrep.Files["danglingRelations"] = danglingRelations(app, b.plans)
	if _, err := app.DB().NewQuery("PRAGMA wal_checkpoint(TRUNCATE)").Execute(); err != nil {
		return res, fmt.Errorf("checkpoint: %w", err)
	}
	if _, err := app.AuxDB().NewQuery("PRAGMA wal_checkpoint(TRUNCATE)").Execute(); err != nil {
		return res, fmt.Errorf("checkpoint: %w", err)
	}
	if err := app.ClearBootstrap(); err != nil {
		return res, err
	}
	since("files", t)

	// 5. upload
	ui.Step(5, steps, "Uploading to PocketBase")
	t = time.Now()
	res.Uploaded = "supabase_migration_" + stamp + ".zip"
	migZip := filepath.Join(rc.work, res.Uploaded)
	if err := zipDir(dataDir, migZip); err != nil {
		return res, err
	}
	st, _ := os.Stat(migZip)
	if err := tg.UploadBackup(migZip); err != nil {
		return res, fmt.Errorf("upload failed: %w", err)
	}
	ui.OK("Uploaded %s (%s)", res.Uploaded, fmtBytes(st.Size()))
	since("upload", t)

	// 6. restore + verify
	ui.Step(6, steps, "Restoring and verifying")
	t = time.Now()
	if err := tg.Restore(res.Uploaded); err != nil {
		return res, fmt.Errorf("restore request failed: %w", err)
	}
	res.Restored = true
	want := map[string]int{}
	for _, cp := range b.plans {
		want[cp.Name] = int(cp.Loaded)
	}
	if b.users != nil {
		want["users"] = mrep.Users["imported"].(int)
	}
	for _, cp := range mrep.Storage {
		want[cp.Name] = int(cp.Loaded)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	res.Verified = map[string][2]int{}
	err = tg.WaitRestored(wctx, rc.email, rc.password, func() (bool, error) {
		for name, n := range want {
			got, err := tg.Count(name)
			if err != nil {
				return false, fmt.Errorf("%s: %w", name, err)
			}
			res.Verified[name] = [2]int{n, got}
			if got != n {
				return false, fmt.Errorf("%s has %d records, expected %d", name, got, n)
			}
		}
		return true, nil
	})
	if err != nil {
		if tg.Health() != nil {
			return res, errors.New(serverDownAfterRestore)
		}
		return res, err
	}
	names := make([]string, 0, len(want))
	total := 0
	for n, c := range want {
		names = append(names, n)
		total += c
	}
	sort.Strings(names)
	ui.OK("Server is back · %d collections · %s records verified", len(names), fmtInt(int64(total)))
	since("restore", t)
	timings["total"] = time.Since(rc.started).Round(100 * time.Millisecond).String()
	return res, nil
}

func sumRows(a *Analysis) int64 {
	var n int64
	for _, t := range a.Tables {
		n += t.Rows
	}
	return n
}

func countObjects(a *Analysis) int64 {
	var n int64
	for _, b := range a.Buckets {
		n += b.Objects
	}
	return n
}

func danglingRelations(app core.App, plans []*CollPlan) map[string]int {
	out := map[string]int{}
	for _, cp := range plans {
		for _, fp := range cp.Fields {
			if fp.Kind == "relation" {
				var n int
				if err := app.DB().NewQuery(fmt.Sprintf("SELECT COUNT(*) FROM `%s` WHERE `%s` != '' AND `%s` NOT IN (SELECT id FROM `%s`)",
					cp.Name, fp.Name, fp.Name, fp.RelTable)).Row(&n); err != nil {
					continue
				}
				if n > 0 {
					out[cp.Name+"."+fp.Name] = n
				}
			}
		}
	}
	return out
}

func writeReport(ui *UI, path string, res *migrationResult) {
	data, _ := json.MarshalIndent(res, "", " ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		ui.Warn("could not write %s: %v", path, err)
	}
}

func nextSteps(ui *UI, res *migrationResult, src *Source, tg *Target, reportPath string) {
	m := res.Migration
	ui.Section(ui.green("Done") + fmt.Sprintf(" in %s · report: %s", m.Timings["total"], reportPath))
	if errs, _ := m.Files["errors"].([]string); len(errs) > 0 {
		ui.Warn("%d file(s) could not be copied (listed in the report)", len(errs))
	}
	if d, _ := m.Files["danglingRelations"].(map[string]int); len(d) > 0 {
		ui.Warn("%d relation field(s) point to missing records (listed in the report)", len(d))
	}
	ui.Section("Next steps")
	n := 0
	step := func(format string, a ...any) {
		n++
		ui.Printf("  %d. %s\n", n, fmt.Sprintf(format, a...))
	}
	authURL := src.AuthURL()
	if authURL == "" {
		authURL = "<your Supabase URL>/auth/v1"
	}
	step("Upload %s to the server's pb_hooks folder and set:\n       SUPABASE_AUTH_URL=%s\n       SUPABASE_PUBLISHABLE_KEY=<publishable key, or the legacy anon key>", res.HooksFile, authURL)
	if len(m.ResetUsers) > 0 {
		step("%d user(s) must reset their password (no portable hash); they are flagged password_reset_required", len(m.ResetUsers))
	}
	var oauth []string
	for _, p := range res.OAuth {
		if p.Supported {
			oauth = append(oauth, p.PocketBase)
		}
	}
	if len(oauth) > 0 {
		step("Configure OAuth2 (%s) on the users collection and add the redirect URL %s/api/oauth2-redirect in each provider console", strings.Join(oauth, ", "), tg.URL)
	}
	var locked, review []string
	for _, cp := range append(append([]*CollPlan{}, m.Plans...), m.Storage...) {
		all, rev := len(cp.Rules) > 0, false
		for _, r := range cp.Rules {
			if r == nil {
				continue
			}
			all = all && r.Status == "locked"
			rev = rev || r.Status == "review"
		}
		if all {
			locked = append(locked, cp.Name)
		} else if rev {
			review = append(review, cp.Name)
		}
	}
	if len(locked) > 0 {
		step("Set API rules for %s locked to superusers (RLS off, no policy, or a policy sb2pb cannot translate; reasons in the report): %s", pl(int64(len(locked)), "collection", "collections"), strings.Join(locked, ", "))
	}
	if len(review) > 0 {
		step("Review the translated rules marked \"review\" in the report for: %s", strings.Join(review, ", "))
	}
	stubs := 0
	for _, h := range m.Hooks {
		if strings.HasPrefix(h, "stub:") {
			stubs++
		}
	}
	if stubs > 0 {
		step("Port %d Postgres function(s) into the generated /api/rpc/* hook stubs", stubs)
	}
	step("Switch the app from supabase-js to the PocketBase SDK and test the main flows")
	ui.Printf("\n  %s\n", ui.dim("Undo: sb2pb rollback --to "+tg.URL))
}
