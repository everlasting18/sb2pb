#!/usr/bin/env bash
# Smoke test against a real hosted Supabase project, which the e2e suite (self-hosted stack) cannot cover:
# the Session pooler, sb_secret_ keys and the hosted Storage API. Supabase is only read. The target is a throwaway
# PocketBase started here, so nothing outside this machine is changed.
#
#   SUPABASE_DB_URL       Session pooler connection string of a test project (required)
#   SUPABASE_SECRET_KEY   secret key, to copy Storage files (optional: without it files are skipped)
#   SB2PB_MIGRATE_ARGS    extra flags for "sb2pb migrate" (e.g. --storage http://localhost:8000/storage/v1)
#
# Use a test project: its table names and row counts are printed. Needs: go, python3.
set -euo pipefail
E2E="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$E2E")"
WORK="${E2E_WORK:-$E2E/.work}/hosted"
: "${SUPABASE_DB_URL:?set SUPABASE_DB_URL to the Session pooler connection string of a test project}"
export PB_URL=http://127.0.0.1:8091 PB_SUPERUSER_EMAIL=smoke@sb2pb.test PB_SUPERUSER_PASSWORD=Smoke-Pass-2026
PB_PID=""

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
cleanup() { [ -z "$PB_PID" ] || kill "$PB_PID" 2>/dev/null || true; }
trap cleanup EXIT

rm -rf "$WORK"; mkdir -p "$WORK/pb"
log "Building sb2pb and a throwaway PocketBase"
(cd "$ROOT" && go build -o "$WORK/sb2pb" ./cmd/sb2pb)
(cd "$E2E/pocketbase" && go build -o "$WORK/pb/pocketbase" .)
(cd "$WORK/pb" && ./pocketbase superuser upsert "$PB_SUPERUSER_EMAIL" "$PB_SUPERUSER_PASSWORD" --dir pb_data > /dev/null)
(cd "$WORK/pb" && exec ./pocketbase serve --http 127.0.0.1:8091 --dir pb_data > "$WORK/pocketbase.log" 2>&1) & PB_PID=$!
for _ in $(seq 1 60); do curl -fsS -o /dev/null "$PB_URL/api/health" 2>/dev/null && break; sleep 1; done
curl -fsS -o /dev/null "$PB_URL/api/health" || { echo "PocketBase did not start; see $WORK/pocketbase.log" >&2; exit 1; }

cd "$WORK"
log "sb2pb analyze (read-only)"
./sb2pb analyze --json readiness.json --html readiness.html

log "sb2pb migrate into the throwaway PocketBase"
args=(migrate --yes --allow-blockers)
[ -n "${SUPABASE_SECRET_KEY:-}" ] || { echo "SUPABASE_SECRET_KEY is not set: Storage files are skipped"; args+=(--skip-files); }
# shellcheck disable=SC2206 # extra flags are split on purpose
args+=(${SB2PB_MIGRATE_ARGS:-})
./sb2pb "${args[@]}"

log "Comparing what Supabase has with what PocketBase received"
python3 - <<'PY'
import json, sys
rep = json.load(open("sb2pb-report.json"))
m, bad = rep["migration"], []
print(f"{'collection':28} {'on Supabase':>12} {'imported':>10} {'on PocketBase':>14}")
for c in m["collections"] + m.get("storageCollections", []):
    if c.get("mergedIntoUsers"):
        continue  # counted with the users below
    src, imp = c["sourceRows"], c["imported"]
    on_pb = (rep.get("verifiedCounts") or {}).get(c["pb"], [None, None])[1]
    print(f"{c['pb']:28} {src:>12} {imp:>10} {str(on_pb):>14}")
    if imp != src or on_pb != imp:
        bad.append(c["pb"])
u = m["users"]
on_pb = (rep.get("verifiedCounts") or {}).get("users", [None, None])[1]
print(f"{'users':28} {u['imported'] + u['skippedAnonymous']:>12} {u['imported']:>10} {str(on_pb):>14}   ({u['skippedAnonymous']} anonymous skipped)")
if on_pb != u["imported"]:
    bad.append("users")
files = m["files"]
print(f"files copied: {files.get('copied', 0)}, skipped: {files.get('skipped', 0)}, errors: {len(files.get('errors') or [])}")
if not rep.get("restored"):
    bad.append("not restored")
if bad:
    sys.exit("MISMATCH: " + ", ".join(bad) + " (rows written to Supabase during the run also show up here)")
print("counts match")
PY

log "sb2pb rollback"
./sb2pb rollback --yes
log "Hosted smoke test passed"
