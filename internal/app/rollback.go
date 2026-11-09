package app

// sb2pb rollback: restore the newest pre_migration_* backup (or a named one) on the PocketBase instance.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
)

func runRollback(ui *UI, args []string) error {
	fs := newFlags("rollback", "Restore the PocketBase instance to the backup sb2pb took before migrating.")
	to := fs.String("to", "", "PocketBase URL (default: $PB_URL, or prompt)")
	email := fs.String("email", "", "superuser email (default: $PB_SUPERUSER_EMAIL, or prompt)")
	backup := fs.String("backup", "", "backup name (default: the newest pre_migration_*.zip)")
	list := fs.Bool("list", false, "only list the available pre-migration backups")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	ui.Title("sb2pb rollback")
	ui.Section("PocketBase")
	tg, em, pw, err := connectTarget(ui, *to, *email, "The PocketBase instance to restore, e.g. https://myapp.pocketbasecloud.com")
	if err != nil {
		return err
	}
	backups, err := tg.Backups()
	if err != nil {
		return fmt.Errorf("cannot list backups: %w", err)
	}
	var pre []pbBackup
	for _, b := range backups {
		if strings.HasPrefix(b.Key, "pre_migration_") {
			pre = append(pre, b)
		}
	}
	sort.Slice(pre, func(i, j int) bool { return pre[i].Key > pre[j].Key }) // names carry a UTC timestamp
	if *list {
		if len(pre) == 0 {
			ui.Info("No pre-migration backups on %s", tg.URL)
		}
		for _, b := range pre {
			ui.Info("%s  %s  %s", b.Key, fmtBytes(b.Size), b.Modified)
		}
		return nil
	}
	var chosen *pbBackup
	if *backup != "" {
		for i := range backups {
			if backups[i].Key == *backup {
				chosen = &backups[i]
			}
		}
		if chosen == nil {
			return fmt.Errorf("backup %q not found on %s (see sb2pb rollback --list)", *backup, tg.URL)
		}
	} else if len(pre) > 0 {
		chosen = &pre[0]
	} else {
		return fmt.Errorf("no pre_migration_* backup found on %s", tg.URL)
	}

	ui.Section("Restore")
	ui.Info("Backup  %s (%s, %s)", ui.bold(chosen.Key), fmtBytes(chosen.Size), chosen.Modified)
	ui.Info("All current data on %s will be replaced by this backup.", tg.URL)
	if !*yes {
		if !ui.interactive {
			return errors.New("not running in a terminal: re-run with --yes to confirm")
		}
		ok, err := ui.Confirm("Restore it?", false)
		if err != nil || !ok {
			return errCancelled
		}
	}
	before, _ := collectionSet(tg)
	restoreKey := chosen.Key
	if key, marked, err := prepareRollback(tg, chosen.Key); err != nil {
		ui.Warn("Could not check pb_migrations against the backup (%v); restoring it as is", err)
	} else if key != "" {
		restoreKey = key
		ui.Info("%s created after this backup %s marked as already applied in the restored data, so PocketBase can start: %s",
			pl(int64(len(marked)), "migration file", "migration files"), map[bool]string{true: "is", false: "are"}[len(marked) == 1], strings.Join(marked, ", "))
	}
	if err := tg.Restore(restoreKey); err != nil {
		return fmt.Errorf("restore request failed: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err = tg.WaitRestored(ctx, em, pw, func() (bool, error) {
		now, err := collectionSet(tg)
		if err != nil {
			return false, err
		}
		return now != before, errors.New("collections unchanged so far")
	})
	if err != nil {
		if tg.Health() != nil {
			return errors.New(serverDownAfterRestore)
		}
		ui.Warn("The restore was requested, but no change was visible within 2 minutes. Check Dashboard → Logs on the server.")
		return nil
	}
	ui.OK("Restored %s · the instance is back to its pre-migration state", chosen.Key)
	return nil
}

// prepareRollback protects against automigrate: with the official PocketBase binary every collection change made in the
// dashboard after the migration writes a JS file to pb_migrations and records it in _migrations. The pre-migration
// backup does not know those files, so after restoring it PocketBase tries to run them, fails (their collections do not
// exist) and does not start. Here the backup is copied with those files recorded as applied, and the copy is restored.
// It returns "" when the backup can be restored as is.
func prepareRollback(tg *Target, key string) (string, []string, error) {
	current, err := tg.AppliedMigrations()
	if err != nil {
		return "", nil, err
	}
	work, err := os.MkdirTemp("", "sb2pb-rollback-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(work)
	zipPath := filepath.Join(work, key)
	if err := tg.DownloadBackup(key, zipPath); err != nil {
		return "", nil, err
	}
	dataDir := filepath.Join(work, "pb_data")
	if err := unzip(zipPath, dataDir); err != nil {
		return "", nil, err
	}
	db, err := core.DefaultDBConnect(filepath.Join(dataDir, "data.db"))
	if err != nil {
		return "", nil, err
	}
	var inBackup []string
	if err := db.NewQuery("SELECT file FROM " + core.DefaultMigrationsTable).Column(&inBackup); err != nil {
		db.Close()
		return "", nil, err
	}
	have := map[string]bool{}
	for _, f := range inBackup {
		have[f] = true
	}
	var missing []string
	for _, f := range current {
		if !have[f] {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		db.Close()
		return "", nil, nil
	}
	now := time.Now().UnixMicro()
	for _, f := range missing {
		if _, err := db.Insert(core.DefaultMigrationsTable, map[string]any{"file": f, "applied": now}).Execute(); err != nil {
			db.Close()
			return "", nil, err
		}
	}
	if _, err := db.NewQuery("PRAGMA wal_checkpoint(TRUNCATE)").Execute(); err != nil {
		db.Close()
		return "", nil, err
	}
	db.Close()
	name := "sb2pb_rollback_" + time.Now().UTC().Format("20060102_150405") + ".zip"
	patched := filepath.Join(work, name)
	if err := zipDir(dataDir, patched); err != nil {
		return "", nil, err
	}
	if err := tg.UploadBackup(patched); err != nil {
		return "", nil, err
	}
	return name, missing, nil
}

func collectionSet(tg *Target) (string, error) {
	cs, err := tg.Collections()
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ","), nil
}
