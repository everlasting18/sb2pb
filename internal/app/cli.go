package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"

	_ "ganigeorgiev.com/pocketbase/pocketbase/migrations" // registers PocketBase's system migrations (version check)
)

// pbVersion is the PocketBase release whose core is embedded; the target server must run the same release.
const pbVersion = "v0.41.1"

const pocketbaseTarget = "PocketBase " + pbVersion

var toolVersion = "dev"

const usage = `sb2pb moves a Supabase project to PocketBase: schema, users (with their passwords),
data, files and access rules.

Usage:
  sb2pb                 interactive migration (same as "sb2pb migrate")
  sb2pb migrate         migrate a Supabase project into a PocketBase instance
  sb2pb analyze         read-only readiness report (nothing is copied or changed)
  sb2pb rollback        restore the PocketBase instance to its pre-migration backup
  sb2pb version         print versions

Run "sb2pb <command> --help" for the options of a command.

Environment variables (used instead of prompts; secrets are never taken as flags):
  SUPABASE_DB_URL         Supabase connection string (Dashboard → Connect → Session pooler)
  SUPABASE_SECRET_KEY     secret key (sb_secret_...) or legacy service_role key, to copy Storage files
  PB_URL                  PocketBase URL
  PB_SUPERUSER_EMAIL      PocketBase superuser email
  PB_SUPERUSER_PASSWORD   PocketBase superuser password

Docs: https://github.com/everlasting18/sb2pb
`

// Main runs the CLI and returns the process exit code.
func Main(args []string, version string) int {
	toolVersion = version
	ui := newUI()
	cmd := ""
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "":
		if !ui.interactive {
			fmt.Print(usage)
			return 2
		}
		err = runMigrate(ui, nil)
	case "migrate":
		err = runMigrate(ui, args)
	case "analyze":
		err = runAnalyze(ui, args)
	case "rollback":
		err = runRollback(ui, args)
	case "version", "--version", "-v":
		fmt.Printf("sb2pb %s (PocketBase %s, %s, %s/%s)\n", toolVersion, embeddedPBVersion(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errCancelled):
		fmt.Fprintln(os.Stderr, "\nCancelled. Nothing was changed on the PocketBase instance.")
		return 130
	}
	fmt.Fprintf(os.Stderr, "\n%s %s\n", ui.red("error:"), err)
	return 1
}

var errCancelled = errors.New("cancelled")

func newFlags(name, summary string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "%s\n\nUsage: sb2pb %s [options]\n\nOptions:\n", summary, name)
		fs.PrintDefaults()
	}
	return fs
}

// parseFlags allows flags after positional-looking values and reports usage errors cleanly.
func parseFlags(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	fs.SetOutput(os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		fs.Usage()
		return err
	}
	if err != nil {
		return fmt.Errorf("%w (see sb2pb %s --help)", err, fs.Name())
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (see sb2pb %s --help)", fs.Arg(0), fs.Name())
	}
	return nil
}

// embeddedPBVersion reads the PocketBase module version from the build info.
func embeddedPBVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if strings.HasSuffix(d.Path, "/pocketbase/pocketbase") {
				return d.Version
			}
		}
	}
	return pbVersion
}
