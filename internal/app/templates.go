package app

const baseCSS = `{{define "css"}}<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<style>
:root{--bg:#f6f7f9;--card:#fff;--ink:#141a26;--muted:#5d6678;--line:#e4e7ec;--soft:#f1f3f6;--brand:#1d4ed8;
--green:#15803d;--green-bg:#e3f6ea;--amber:#9a5b00;--amber-bg:#fff2d9;--red:#b42318;--red-bg:#fde7e5;--blue:#1e40af;--blue-bg:#e6edff;--code:#eef1f5}
@media (prefers-color-scheme: dark){:root{--bg:#0d1117;--card:#151b24;--ink:#e6e9ef;--muted:#9aa4b5;--line:#263040;--soft:#1b2330;--brand:#7aa2ff;
--green:#4ade80;--green-bg:#12301e;--amber:#fbbf24;--amber-bg:#33270c;--red:#f87171;--red-bg:#3a1513;--blue:#93b4ff;--blue-bg:#16233f;--code:#202a38}}
*{box-sizing:border-box}html{-webkit-text-size-adjust:100%}
body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Inter,Roboto,sans-serif}
a{color:var(--brand)}code{background:var(--code);padding:1px 5px;border-radius:5px;font:12.5px ui-monospace,SFMono-Regular,Menlo,monospace;word-break:break-word}
.top{display:flex;justify-content:space-between;align-items:center;padding:14px 24px;border-bottom:1px solid var(--line);background:var(--card)}
.brand{font-weight:700;letter-spacing:-.01em}.brand span{font-weight:500;color:var(--muted);margin-left:6px}
main{max-width:1080px;margin:0 auto;padding:24px 16px 60px}
h1{font-size:28px;line-height:1.2;margin:2px 0 6px;letter-spacing:-.02em;word-break:break-word}h2{font-size:19px;margin:34px 0 12px;letter-spacing:-.01em}
.muted{color:var(--muted)}.eyebrow{text-transform:uppercase;letter-spacing:.08em;font-size:12px;color:var(--muted);margin:0}
.card{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:18px}
.btn{display:inline-block;background:var(--brand);color:#fff;border:0;border-radius:10px;padding:11px 18px;font:600 15px inherit;text-decoration:none;cursor:pointer}
.btn.ghost{background:transparent;color:var(--brand);border:1px solid var(--line)}
.chip{display:inline-block;font-size:12px;font-weight:600;padding:2px 9px;border-radius:99px;white-space:nowrap}
.blocker,.red{background:var(--red-bg);color:var(--red)}.action,.amber,.review{background:var(--amber-bg);color:var(--amber)}
.ok,.green,.translated{background:var(--green-bg);color:var(--green)}.info,.locked{background:var(--blue-bg);color:var(--blue)}
.scroll{overflow-x:auto;-webkit-overflow-scrolling:touch}
table{border-collapse:collapse;width:100%}th,td{text-align:left;padding:8px 10px;border-bottom:1px solid var(--line);vertical-align:top;font-size:13.5px}
th{font-size:12px;color:var(--muted);font-weight:600;text-transform:uppercase;letter-spacing:.04em}
.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12.5px}
</style>{{end}}`

const reportHTML = `<!doctype html><html lang="en"><head>{{template "css"}}<title>Readiness report · {{.Label}}</title>
<style>
.hero{display:grid;grid-template-columns:170px 1fr;gap:26px;align-items:center}
.gauge{width:170px;height:170px;border-radius:50%;display:grid;place-items:center;background:conic-gradient(var(--c) calc(var(--p)*1%),var(--soft) 0)}
.gauge.green{--c:var(--green)}.gauge.amber{--c:var(--amber)}.gauge.red{--c:var(--red)}
.gauge>div{width:132px;height:132px;border-radius:50%;background:var(--bg);display:grid;place-items:center;text-align:center;line-height:1}
.gauge b{font-size:44px;letter-spacing:-.03em}.gauge span{display:block;color:var(--muted);font-size:13px;margin-top:4px}
.verdict{display:inline-block;font-weight:700;padding:5px 12px;border-radius:99px;margin:8px 0 4px}
.facts{display:grid;grid-template-columns:repeat(4,1fr);gap:10px;margin-top:14px}.facts div{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:10px 12px}
.facts span{display:block;font-size:12px;color:var(--muted)}.facts b{font-size:18px}
.cards{display:grid;grid-template-columns:repeat(5,1fr);gap:12px;margin-top:22px}.cards .card{padding:14px}
.cards h3{margin:0 0 6px;font-size:13px;color:var(--muted);font-weight:600;text-transform:uppercase;letter-spacing:.05em}.cards b{font-size:21px;display:block}
.cards p{margin:2px 0 0;font-size:13px;color:var(--muted)}
.item{display:grid;grid-template-columns:96px 1fr;gap:12px;padding:14px 0;border-bottom:1px solid var(--line)}.item:last-child{border:0}
.item h4{margin:0 0 4px;font-size:15px;word-break:break-word}.item p{margin:0 0 4px;color:var(--muted);font-size:14px;word-break:break-word}.item .how{color:var(--ink)}
.area{font-size:12px;color:var(--muted);margin-top:6px}
details{background:var(--card);border:1px solid var(--line);border-radius:12px;margin-bottom:10px}
summary{cursor:pointer;padding:12px 14px;display:flex;gap:10px;align-items:center;flex-wrap:wrap;list-style:none}summary::-webkit-details-marker{display:none}
summary b{font-family:ui-monospace,Menlo,monospace}details .inner{padding:0 14px 14px}
.warn{color:var(--amber);font-weight:600}.cta{display:flex;justify-content:space-between;align-items:center;gap:16px;flex-wrap:wrap}
ul.plain{margin:0;padding-left:18px}ul.plain li{margin:5px 0}
@media (max-width:860px){.cards{grid-template-columns:repeat(2,1fr)}.facts{grid-template-columns:repeat(2,1fr)}}
@media (max-width:620px){.hero{grid-template-columns:1fr;justify-items:start}.gauge{width:140px;height:140px}.gauge>div{width:108px;height:108px}.gauge b{font-size:36px}
.item{grid-template-columns:1fr;gap:4px}h1{font-size:24px}.top{padding:12px 16px}.brand span{display:none}.top span.muted{display:none}}
</style></head><body>
<div class="top"><div class="brand">sb2pb<span>Supabase → PocketBase readiness</span></div><span class="muted" style="font-size:13px">{{.Tool}}</span></div>
<main>
<section class="hero">
 <div class="gauge {{.VerdictKey}}" style="--p:{{.Score}}"><div><div><b>{{.Score}}</b><span>out of 100</span></div></div></div>
 <div>
  <p class="eyebrow">Supabase → {{.Target}}</p>
  <h1>{{.Label}}</h1>
  <div class="muted" style="font-size:13.5px">{{.Host}} · Postgres {{.PGVersion}} · {{when .AnalyzedAt}} · analyzed in {{.DurationMs}} ms</div>
  <div class="verdict {{.VerdictKey}}">{{.Verdict}}</div>
  <div>{{.VerdictText}}</div>
  <div class="facts">
   <div><span>Estimated work</span><b>{{hours .EffortLow}}–{{hours .EffortHigh}} h</b></div>
   <div><span>Migration run</span><b>{{dur .MigrateSecs}}</b></div>
   <div><span>Blockers</span><b>{{.Count "blocker"}}</b></div>
   <div><span>Action items</span><b>{{.Count "action"}}</b></div>
  </div>
 </div>
</section>

<section class="cards">
 <div class="card"><h3>Data</h3><b>{{int .Stats.Rows}}</b><p>rows in {{.Stats.Tables}} tables · {{bytes .Stats.DBBytes}}</p></div>
 <div class="card"><h3>Users</h3><b>{{int .Stats.Users}}</b><p>{{pct .Stats.UsersKeepPassword .Stats.Users}}% keep their password</p></div>
 <div class="card"><h3>Storage</h3><b>{{int .Stats.Objects}}</b><p>files · {{bytes .Stats.StorageBytes}} in {{.Stats.Buckets}} buckets</p></div>
 <div class="card"><h3>Access rules</h3><b>{{.Stats.PoliciesAuto}}/{{.Stats.Policies}}</b><p>auto-translated · {{.Stats.PoliciesReview}} to review</p></div>
 <div class="card"><h3>Server logic</h3><b>{{.Stats.Functions}} + {{.Stats.Triggers}}</b><p>functions + triggers to port</p></div>
</section>

<h2>What needs attention</h2>
<div class="card">
{{range $sev := (list "blocker" "action" "info" "ok")}}{{range $.BySeverity $sev}}
 <div class="item"><div><span class="chip {{.Severity}}">{{if eq .Severity "blocker"}}Blocker{{else if eq .Severity "action"}}Action{{else if eq .Severity "info"}}Note{{else}}Automatic{{end}}</span><div class="area">{{.Area}}</div></div>
 <div><h4>{{md .Title}}</h4><p>{{md .Detail}}</p><p class="how"><b>{{if eq .Severity "ok"}}Handled:{{else}}What to do:{{end}}</b> {{md .Handling}}{{if .Effort}} <span class="muted">· ~{{hours .Effort}} h</span>{{end}}</p></div></div>
{{end}}{{end}}
</div>

<h2>Behavior differences your app code must handle</h2>
<div class="card">{{range .Differences}}<div class="item"><div><span class="chip info">{{.Area}}</span></div><div><h4>{{md .Title}}</h4><p>{{md .Detail}}</p><p class="how"><b>What to do:</b> {{md .Handling}}</p></div></div>{{end}}</div>

<h2>Effort estimate</h2>
<div class="card scroll"><table><tr><th>Work item</th><th style="width:90px">Hours</th></tr>
{{range .Effort}}<tr><td>{{md .Label}}</td><td>{{hours .Hours}}</td></tr>{{end}}
<tr><td><b>Range (×0.7 – ×1.5)</b></td><td><b>{{hours .EffortLow}}–{{hours .EffortHigh}}</b></td></tr></table>
<p class="muted" style="font-size:13px;margin:10px 0 0">A rough guide for one developer who knows the app. The migration run itself ({{dur .MigrateSecs}}) is automated.</p></div>

<h2>Tables → collections</h2>
{{range .Tables}}<details><summary><b>{{.PG}}</b> → <b>{{.PB}}</b>{{if .Merged}} <span class="chip info">merged into users</span>{{end}}
 <span class="muted">{{int .Rows}} rows{{if .Sampled}} (est.){{end}} · id: {{.IdMode}}</span>{{if not .RLS}} <span class="chip action">RLS off</span>{{end}}{{if .Warnings}} <span class="chip action">{{.Warnings}} to check</span>{{end}}</summary>
 <div class="inner"><div class="scroll"><table><tr><th>Postgres column</th><th>Type</th><th>PocketBase field</th><th>Notes</th></tr>
 {{range .Fields}}<tr><td class="mono">{{.PG}}</td><td class="mono">{{.PGType}}</td><td class="mono">{{.PB}} : {{.Kind}}</td><td{{if .Warn}} class="warn"{{end}}>{{.Note}}</td></tr>{{end}}</table></div>
 <div class="scroll" style="margin-top:12px"><table><tr><th>Operation</th><th>Rule</th><th>Status</th></tr>
 {{range .Rules}}<tr><td>{{.Cmd}}</td><td class="mono">{{if eq .Status "locked"}}superusers only{{if .Reason}} — <span class="muted">{{.Reason}}</span>{{end}}{{else if eq .Rule ""}}public{{else}}{{.Rule}}{{end}}</td><td><span class="chip {{.Status}}">{{.Status}}</span></td></tr>{{end}}</table></div></div></details>{{end}}

{{if .HasAuth}}<h2>Users &amp; sign-in</h2>
<div class="card scroll"><table><tr><th>Users</th><th>Keep password</th><th>Reset needed</th><th>Anonymous (skipped)</th><th>Phone only</th><th>Banned</th></tr>
<tr><td>{{int .Auth.Total}}</td><td>{{int .Auth.Bcrypt}}</td><td>{{.Auth.Argon2}} argon2 · {{.Auth.FbScrypt}} scrypt · {{.Auth.NoPassword}} no password</td><td>{{.Auth.Anonymous}}</td><td>{{.Auth.PhoneOnly}}</td><td>{{.Auth.Banned}}</td></tr></table>
{{if .Providers}}<table style="margin-top:14px"><tr><th>Supabase provider</th><th>PocketBase provider</th><th>Linked users</th><th></th></tr>
{{range .Providers}}<tr><td class="mono">{{.Supabase}}</td><td class="mono">{{.PocketBase}}</td><td>{{.Users}}</td><td>{{if .Supported}}<span class="chip ok">supported</span>{{else}}<span class="chip blocker">no equivalent</span>{{end}}</td></tr>{{end}}</table>{{end}}</div>{{end}}

{{if .Buckets}}<h2>Storage</h2>
<div class="card scroll"><table><tr><th>Bucket</th><th>Access</th><th>Files</th><th>Size</th><th>Largest file</th></tr>
{{range .Buckets}}<tr><td class="mono">{{.ID}}</td><td>{{if .Public}}public{{else}}private → protected files{{end}}</td><td>{{int .Objects}}</td><td>{{bytes .Bytes}}</td><td>{{bytes .MaxSize}}</td></tr>{{end}}</table></div>{{end}}

<h2>Database logic</h2>
<div class="card"><ul class="plain">
<li><b>Functions:</b> {{if .Functions}}{{range $i, $f := .Functions}}{{if $i}}, {{end}}<code>{{$f.Name}}({{$f.Args}})</code>{{end}}{{else}}none{{end}}</li>
<li><b>Triggers:</b> {{if .Triggers}}{{range $i, $t := .Triggers}}{{if $i}}, {{end}}<code>{{$t.Name}}</code> on {{$t.Schema}}.{{$t.Table}}{{end}}{{else}}none{{end}}</li>
<li><b>Views:</b> {{if .Views}}{{join .Views ", "}}{{else}}none{{end}}</li>
<li><b>Realtime tables:</b> {{if .Realtime}}{{join .Realtime ", "}}{{else}}none{{end}}</li>
<li><b>Extensions installed:</b> <span class="muted">{{join .Extensions ", "}}</span></li></ul></div>

<h2>How this report was produced</h2>
<div class="card"><ul class="plain">{{range .Safety}}<li>{{.}}</li>{{end}}</ul></div>

<div class="card cta" style="margin-top:28px"><div><b>Ready to move?</b><div class="muted">The migration repeats this analysis on a consistent snapshot, then imports data, users, files and rules into your PocketBase instance. A backup of the instance is taken first, so you can roll back.</div></div>
<div style="min-width:0;max-width:100%"><code style="display:block;padding:10px 12px;overflow-x:auto;white-space:nowrap">sb2pb migrate</code></div></div>
</main></body></html>`
