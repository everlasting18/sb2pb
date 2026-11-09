package app

// Target side: PocketBase superuser API (auth, backups, restore) and the version check.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
)

type Target struct {
	URL   string
	token string
	http  *http.Client // short calls
	bulk  *http.Client // backup download/upload: no overall timeout
}

// NewTarget normalises the PocketBase URL (https:// is assumed when no scheme is given).
func NewTarget(raw string) (*Target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("the PocketBase URL is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%q is not a valid PocketBase URL", raw)
	}
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/_")
	u.RawQuery, u.Fragment = "", ""
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 10 * time.Minute}
	return &Target{URL: strings.TrimRight(u.String(), "/"), http: &http.Client{Timeout: 60 * time.Second, Transport: tr},
		bulk: &http.Client{Transport: tr}}, nil
}

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

func (t *Target) do(c *http.Client, method, p string, body io.Reader, ctype string, out any) error {
	req, err := http.NewRequest(method, t.URL+p, body)
	if err != nil {
		return err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if t.token != "" {
		req.Header.Set("Authorization", t.token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var pe struct{ Message string }
		_ = json.Unmarshal(data, &pe) // error bodies are not always JSON
		if pe.Message == "" {
			pe.Message = strings.TrimSpace(string(data))
		}
		return &apiError{resp.StatusCode, pe.Message}
	}
	if out == nil {
		return nil
	}
	if w, ok := out.(io.Writer); ok {
		_, err = io.Copy(w, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (t *Target) jsonDo(method, p string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	return t.do(t.http, method, p, body, "application/json", out)
}

// Health checks that the URL answers like a PocketBase server.
func (t *Target) Health() error {
	var h struct {
		Code    int
		Message string
	}
	err := t.jsonDo("GET", "/api/health", nil, &h)
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		return fmt.Errorf("%s does not look like a PocketBase server (/api/health returned %d)", t.URL, ae.Status)
	case err != nil:
		return fmt.Errorf("cannot reach %s: %w", t.URL, err)
	case h.Code != 200:
		return fmt.Errorf("%s does not look like a PocketBase server", t.URL)
	}
	return nil
}

func (t *Target) Login(email, password string) error {
	var auth struct{ Token string }
	err := t.jsonDo("POST", "/api/collections/_superusers/auth-with-password", map[string]string{"identity": email, "password": password}, &auth)
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.Status {
		case 400:
			return errors.New("superuser login failed: wrong email or password")
		case 404:
			return errors.New("superuser login is not available: sb2pb needs PocketBase v0.23 or newer")
		}
	}
	if err != nil {
		return fmt.Errorf("superuser login failed: %w", err)
	}
	t.token = auth.Token
	return nil
}

type pbCollection struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	System bool   `json:"system"`
}

func (t *Target) Collections() ([]pbCollection, error) {
	var res struct{ Items []pbCollection }
	err := t.jsonDo("GET", "/api/collections?perPage=500&skipTotal=1", nil, &res)
	return res.Items, err
}

func (t *Target) Count(coll string) (int, error) {
	var res struct{ TotalItems int }
	err := t.jsonDo("GET", "/api/collections/"+url.PathEscape(coll)+"/records?perPage=1&fields=id", nil, &res)
	return res.TotalItems, err
}

// CheckEmpty requires a fresh instance: only system collections and an empty `users` collection.
func (t *Target) CheckEmpty() error {
	colls, err := t.Collections()
	if err != nil {
		return fmt.Errorf("cannot list collections: %w", err)
	}
	var extra []string
	hasUsers := false
	for _, c := range colls {
		switch {
		case c.Name == "users" && c.Type == "auth":
			hasUsers = true
		case c.System:
		default:
			extra = append(extra, c.Name)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return fmt.Errorf("the target is not empty (collections: %s). sb2pb replaces everything on the target, so use a fresh PocketBase instance", strings.Join(extra, ", "))
	}
	if !hasUsers {
		return errors.New("the target has no `users` auth collection; create a fresh PocketBase instance (it comes with one)")
	}
	n, err := t.Count("users")
	if err != nil {
		return fmt.Errorf("cannot count users: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("the target already has %d user(s); use a fresh PocketBase instance", n)
	}
	return nil
}

// AppliedMigrations reads the server's _migrations table through the superuser SQL API.
func (t *Target) AppliedMigrations() ([]string, error) {
	var res struct {
		Rows [][]any `json:"rows"`
	}
	if err := t.jsonDo("POST", "/api/sql", map[string]string{"query": "SELECT file FROM " + core.DefaultMigrationsTable + " ORDER BY file"}, &res); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.Rows))
	for _, r := range res.Rows {
		if len(r) > 0 {
			if f, ok := r[0].(string); ok {
				out = append(out, f)
			}
		}
	}
	return out, nil
}

type pbBackup struct {
	Key      string `json:"key"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
}

func (t *Target) Backups() ([]pbBackup, error) {
	var list []pbBackup
	err := t.jsonDo("GET", "/api/backups", nil, &list)
	return list, err
}

func (t *Target) CreateBackup(name string) error {
	return t.jsonDo("POST", "/api/backups", map[string]string{"name": name}, nil)
}

func (t *Target) DownloadBackup(name, dst string) error {
	var ft struct{ Token string }
	if err := t.jsonDo("POST", "/api/files/token", nil, &ft); err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if err := t.do(t.bulk, "GET", "/api/backups/"+url.PathEscape(name)+"?token="+url.QueryEscape(ft.Token), nil, "", f); err != nil {
		f.Close()
		return err
	}
	return f.Close() // a failed close means the backup on disk is incomplete
}

// UploadBackup streams a zip to /api/backups/upload without loading it in memory.
func (t *Target) UploadBackup(src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", filepath.Base(src))
		if err == nil {
			_, err = io.Copy(part, f)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	return t.do(t.bulk, "POST", "/api/backups/upload", pr, mw.FormDataContentType(), nil)
}

func (t *Target) Restore(name string) error {
	return t.jsonDo("POST", "/api/backups/"+url.PathEscape(name)+"/restore", nil, nil)
}

// PocketBase restarts itself after a restore. When it does not come back, the usual cause is a JS migration in
// pb_migrations that was created after the backup (automigrate, i.e. dev mode) and no longer applies.
const serverDownAfterRestore = "PocketBase restored the backup but did not start again (it is not answering). Check its log. " +
	"A common cause is a file in pb_migrations created after this backup, for example by automigrate in development: " +
	"move those files out of pb_migrations and start PocketBase again."

// WaitRestored waits for the server to restart after a restore and for `ok` to report the new state.
func (t *Target) WaitRestored(ctx context.Context, email, password string, ok func() (bool, error)) error {
	time.Sleep(2 * time.Second) // PocketBase starts the restore ~1 s after answering
	var last error
	for {
		if t.Health() == nil && t.Login(email, password) == nil {
			done, err := ok()
			if done {
				return nil
			}
			last = err
		}
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("the server did not come back with the restored data: %w", last)
			}
			return errors.New("the server did not come back with the restored data in time; check the PocketBase logs (Dashboard → Logs)")
		case <-time.After(time.Second):
		}
	}
}

// ---------- version check ----------

// checkVersion compares the system migrations recorded in the target's data with the ones built
// into sb2pb. The data is built offline with the embedded PocketBase core and restored on the
// server, so both must share the same data format (same system migrations). An older release with
// identical system migrations is accepted; it reads the restored data the same way.
func checkVersion(dataDir string) error {
	db, err := core.DefaultDBConnect(filepath.Join(dataDir, "data.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	var applied []string
	if err := db.NewQuery("SELECT file FROM " + core.DefaultMigrationsTable).Column(&applied); err != nil {
		return fmt.Errorf("cannot read the target's migrations: %w", err)
	}
	have := map[string]bool{}
	for _, f := range applied {
		have[f] = true
	}
	known := map[string]bool{}
	var missing []string
	for _, m := range core.SystemMigrations.Items() {
		known[m.File] = true
		if !have[m.File] {
			missing = append(missing, m.File)
		}
	}
	var newer []string
	for _, f := range applied {
		if strings.HasSuffix(f, ".go") && !known[f] {
			newer = append(newer, f)
		}
	}
	switch {
	case len(newer) > 0:
		return fmt.Errorf("the server has Go migrations sb2pb %s does not know: %s. Either it runs a newer PocketBase than %s (use a newer sb2pb or run the server on %s), "+
			"or these are your own Go migrations in a custom build (then re-run with --skip-version-check)",
			toolVersion, strings.Join(newer, ", "), pbVersion, pbVersion)
	case len(missing) > 0:
		return fmt.Errorf("the server runs an older PocketBase than %s (missing system migrations: %s). Upgrade the server to %s first",
			pbVersion, strings.Join(missing, ", "), pbVersion)
	}
	return nil
}

// ---------- zip helpers ----------

func unzip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	root := filepath.Clean(dst) + string(os.PathSeparator)
	for _, f := range r.File {
		p := filepath.Join(dst, f.Name) //nolint:gosec // checked against root on the next line
		if !strings.HasPrefix(p, root) {
			return fmt.Errorf("unsafe path in backup: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := extract(f, p); err != nil {
			return err
		}
	}
	return nil
}

func extract(f *zip.File, p string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	w, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, rc); err != nil { //nolint:gosec // the backup comes from the user's own PocketBase server
		w.Close()
		return err
	}
	return w.Close()
}

// zipDir packs pb_data the way PocketBase backups are laid out (no local backups, no WAL files).
func zipDir(src, dst string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(out)
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if rel == core.LocalBackupsDirName {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(rel, "-shm") || strings.HasSuffix(rel, "-wal") {
			return nil
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		f, err := os.Open(p) //nolint:gosec // our own private temp folder
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if err != nil {
		out.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
