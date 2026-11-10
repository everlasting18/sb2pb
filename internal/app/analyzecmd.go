package app

// sb2pb analyze: read-only readiness report in the terminal, as HTML and as JSON.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func runAnalyze(ui *UI, args []string) error {
	fs := newFlags("analyze", "Read-only readiness report: what migrates automatically, what needs changes, and how much work is left.")
	from := fs.String("from", "", "Supabase connection string (default: $SUPABASE_DB_URL, or prompt)")
	htmlOut := fs.String("html", "", "write the report as an HTML page, e.g. report.html")
	jsonOut := fs.String("json", "", "write the report as JSON, e.g. report.json")
	sample := fs.Int64("sample-rows", 50000, "profile tables larger than this on a TABLESAMPLE (0 = always read everything)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	ui.Title("sb2pb analyze · Supabase → " + pocketbaseTarget)
	src, conn, err := connectSource(ui, *from, analyzeSession)
	if err != nil {
		return err
	}
	ctx := context.Background()
	defer conn.Close(ctx)

	start := time.Now()
	a, err := analyze(ctx, conn, AnalyzeOptions{SampleRows: *sample})
	if err != nil {
		return fmt.Errorf("analysis failed: %s", friendly(err))
	}
	rep := readinessFor(src, a, start)
	printSummary(ui, rep)
	printAttention(ui, rep, 0)

	if *jsonOut != "" {
		data, _ := json.MarshalIndent(rep, "", " ")
		if err := os.WriteFile(*jsonOut, data, 0o644); err != nil {
			return err
		}
		ui.OK("JSON report written to %s", *jsonOut)
	}
	if *htmlOut != "" {
		f, err := os.Create(*htmlOut)
		if err != nil {
			return err
		}
		if err := renderHTML(f, rep); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		ui.OK("HTML report written to %s", *htmlOut)
	}
	if *htmlOut == "" && *jsonOut == "" {
		ui.Printf("\n  %s\n", ui.dim("Full report with every table, field and rule: sb2pb analyze --html report.html"))
	}
	ui.Printf("  %s\n", ui.dim("Next: sb2pb migrate"))
	return nil
}

// connectSource resolves the connection string (flag, env, prompt) and opens a read-only session.
// On an interactive terminal a wrong string or password can be re-entered.
func connectSource(ui *UI, flagVal, session string) (*Source, *pgx.Conn, error) {
	ui.Section("Supabase")
	prompted := flagVal == "" && os.Getenv("SUPABASE_DB_URL") == ""
	for attempt := 1; ; attempt++ {
		dsn, err := ui.value(flagVal, []string{"SUPABASE_DB_URL"}, func() (string, error) {
			return ui.Secret("Connection string", "Supabase Dashboard → Connect → Session pooler (input is hidden)")
		})
		if errors.Is(err, errNoInput) {
			return nil, nil, fmt.Errorf("no connection string: pass --from or set SUPABASE_DB_URL")
		}
		if err != nil {
			return nil, nil, err
		}
		src, err := ParseSource(dsn)
		if err == nil {
			ui.Info("%s", ui.dim(src.Redacted()))
			if flagVal != "" && src.Config.Password != "" {
				ui.Warn("--from puts the database password in your shell history and the process list; prefer SUPABASE_DB_URL or the hidden prompt")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			var conn *pgx.Conn
			conn, err = src.Connect(ctx, session)
			cancel()
			if err == nil {
				var ro, ver string
				qerr := conn.QueryRow(context.Background(), "select current_setting('transaction_read_only'), split_part(current_setting('server_version'), ' ', 1)").Scan(&ro, &ver)
				if qerr == nil {
					project := src.Ref
					if project == "" {
						project = src.Host
					}
					ui.OK("Connected · project %s · Postgres %s · read-only session (transaction_read_only = %s)", project, ver, ro)
					return src, conn, nil
				}
				conn.Close(context.Background()) // connected, but the first query failed (e.g. a transaction-mode pooler)
				err = errors.New(friendly(qerr))
			}
		}
		if !prompted || attempt >= 3 {
			return nil, nil, err
		}
		ui.Warn("%s", err)
	}
}

func readinessFor(src *Source, a *Analysis, start time.Time) *Readiness {
	rep := buildReadiness(a)
	rep.Project, rep.Host, rep.Database = src.Ref, src.Host, src.Database
	if rep.Project == "" {
		rep.Project = src.Host
	}
	rep.Label = rep.Project
	rep.AnalyzedAt, rep.DurationMs = time.Now().UTC(), time.Since(start).Milliseconds()
	rep.Safety = []string{
		fmt.Sprintf("The session ran with default_transaction_read_only = on (verified: transaction_read_only = %s), so Postgres refused any write.", a.ReadOnly),
		"Only catalog queries and aggregate reads (counts, max length, NULL counts) were executed; no rows were copied out.",
		"The connection string was used for this session only. This report contains no credentials.",
	}
	var sampled []string
	for _, t := range a.Tables {
		if t.Sampled {
			sampled = append(sampled, fmt.Sprintf("%s (%g%% sample of ~%s rows)", t.Name, t.SamplePct, fmtInt(t.EstRows)))
		}
	}
	if len(sampled) > 0 {
		rep.Safety = append(rep.Safety, "Large tables were profiled on a TABLESAMPLE to keep the load low: "+strings.Join(sampled, ", ")+". Row counts for them are estimates; the migration reads everything.")
	}
	rep.Safety = append(rep.Safety, fmt.Sprintf("Mapping rules are those of sb2pb %s for %s.", toolVersion, pocketbaseTarget))
	return rep
}

var reTicks = regexp.MustCompile("`([^`]+)`")

func plain(s string) string { return reTicks.ReplaceAllString(s, "$1") }

func printSummary(ui *UI, r *Readiness) {
	s := r.Stats
	ui.Section("Readiness")
	row := func(k, v string) { ui.Printf("  %-13s %s\n", k, v) }
	row("Data", fmt.Sprintf("%s · %s rows · %s", pl(int64(s.Tables), "table", "tables"), fmtInt(s.Rows), fmtBytes(s.DBBytes)))
	if r.HasAuth {
		row("Users", fmt.Sprintf("%s (%s keep their password)", fmtInt(s.Users), fmtInt(s.UsersKeepPassword)))
	}
	if r.HasStorage {
		row("Storage", fmt.Sprintf("%s files · %s in %s", fmtInt(s.Objects), fmtBytes(s.StorageBytes), pl(int64(s.Buckets), "bucket", "buckets")))
	}
	if s.Policies > 0 {
		row("Access rules", fmt.Sprintf("%d of %d policies translated automatically", s.PoliciesAuto, s.Policies))
	}
	if s.Views > 0 || s.Functions > 0 || s.Triggers > 0 {
		row("Logic", fmt.Sprintf("%s · %s · %s", pl(int64(s.Views), "view", "views"), pl(int64(s.Functions), "function", "functions"), pl(int64(s.Triggers), "trigger", "triggers")))
	}
	verdict := r.Verdict
	switch r.VerdictKey {
	case "green":
		verdict = ui.green(verdict)
	case "amber":
		verdict = ui.amber(verdict)
	case "red":
		verdict = ui.red(verdict)
	}
	row("Score", fmt.Sprintf("%s · %s · ~%s–%s h of follow-up work", ui.bold(fmt.Sprintf("%d/100", r.Score)), verdict, hours(r.EffortLow), hours(r.EffortHigh)))
}

// printAttention lists blockers and action items (limit 0 = all).
func printAttention(ui *UI, r *Readiness, limit int) {
	b, a := r.BySeverity("blocker"), r.BySeverity("action")
	if len(b)+len(a) == 0 {
		ui.OK("Nothing needs attention before migrating")
		return
	}
	ui.Printf("\n")
	shown := 0
	for _, it := range b {
		ui.Printf("  %s %s\n    %s\n", ui.red("blocker"), plain(it.Title), ui.dim(plain(it.Handling)))
		shown++
	}
	for _, it := range a {
		if limit > 0 && shown >= limit {
			ui.Printf("  %s\n", ui.dim(fmt.Sprintf("… and %d more action items (sb2pb analyze --html report.html)", len(b)+len(a)-shown)))
			break
		}
		ui.Printf("  %s  %s\n", ui.amber("action"), plain(it.Title))
		shown++
	}
}

func hours(h float64) string { return strconv.FormatFloat(math.Round(h*10)/10, 'f', -1, 64) }

// ---------- HTML ----------

func (r *Readiness) Count(sev string) int { return len(r.BySeverity(sev)) }

func (r *Readiness) BySeverity(sev string) []RItem {
	var out []RItem
	for _, it := range r.Items {
		if it.Severity == sev {
			out = append(out, it)
		}
	}
	return out
}

var reportTmpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"int": fmtInt, "bytes": fmtBytes, "dur": fmtDuration, "lower": strings.ToLower,
	"hours": func(h float64) string { return strconv.FormatFloat(math.Round(h*100)/100, 'f', -1, 64) },
	"when":  func(t time.Time) string { return t.UTC().Format("2 Jan 2006, 15:04 UTC") },
	"pct": func(a, b int64) int64 {
		if b == 0 {
			return 0
		}
		return a * 100 / b
	},
	"md": func(s string) template.HTML {
		return template.HTML(reTicks.ReplaceAllString(template.HTMLEscapeString(s), "<code>$1</code>")) //nolint:gosec // escaped first
	},
	"join": strings.Join, "list": func(v ...string) []string { return v },
}).Parse(baseCSS + reportHTML))

func renderHTML(w io.Writer, r *Readiness) error {
	return reportTmpl.Execute(w, struct {
		*Readiness
		Tool string
	}{r, "sb2pb " + toolVersion})
}
