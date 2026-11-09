#!/usr/bin/env bash
# End-to-end test: real self-hosted Supabase -> sb2pb migrate -> PocketBase, then 52 behaviour checks,
# a 265-cell data type matrix and a rollback. Needs: docker (compose v2), go, python3 + e2e/requirements.txt.
#   e2e/run.sh            run everything, then tear the stack down
#   KEEP=1 e2e/run.sh     leave Supabase and PocketBase running afterwards
set -euo pipefail
E2E="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$E2E")"
export E2E_WORK="${E2E_WORK:-$E2E/.work}"
COMPOSE=(docker compose -p sb2pb-e2e -f "$E2E/supabase/compose.yml" --env-file "$E2E/supabase/demo.env")
export PB_URL=http://127.0.0.1:8090 PB_SUPERUSER_EMAIL=owner@pocketbasecloud.test PB_SUPERUSER_PASSWORD=Owner-Pass-2026
DEMO_PW=$(grep '^POSTGRES_PASSWORD=' "$E2E/supabase/demo.env" | cut -d= -f2)
SERVICE_KEY=$(grep '^SERVICE_ROLE_KEY=' "$E2E/supabase/demo.env" | cut -d= -f2)
ANON_KEY=$(grep '^ANON_KEY=' "$E2E/supabase/demo.env" | cut -d= -f2)
PIDS=()

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
cleanup() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  if [ "${KEEP:-}" != "1" ]; then "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT
wait_http() { # url, name
  for _ in $(seq 1 120); do curl -fsS -o /dev/null "$1" 2>/dev/null && return 0; sleep 2; done
  echo "timeout waiting for $2 ($1)" >&2; return 1
}

rm -rf "$E2E_WORK"; mkdir -p "$E2E_WORK/pb/pb_hooks"

log "Building sb2pb and the PocketBase test server"
(cd "$ROOT" && go build -o "$E2E_WORK/sb2pb" ./cmd/sb2pb)
(cd "$E2E/pocketbase" && go build -o "$E2E_WORK/pb/pocketbase" .)

log "Starting Supabase (db, auth, storage, rest, api gateway) and the mock OIDC provider"
python3 "$E2E/mock_idp.py" & PIDS+=($!)
"${COMPOSE[@]}" up -d db auth rest storage api-gw
wait_http http://localhost:9999/health "Supabase Auth"
wait_http http://localhost:5000/status "Supabase Storage"
wait_http http://localhost:8000/storage/v1/version "API gateway" || true

log "Loading the fixture schema and seeding through the Supabase APIs"
"${COMPOSE[@]}" exec -T db psql -q -v ON_ERROR_STOP=1 -U postgres -d postgres < "$E2E/fixtures/schema.sql"
"${COMPOSE[@]}" exec -T db psql -q -v ON_ERROR_STOP=1 -U postgres -d postgres < "$E2E/fixtures/typelab.sql"
python3 "$E2E/fixtures/seed.py"

log "Starting an empty PocketBase and configuring it like a customer would"
(cd "$E2E_WORK/pb" && ./pocketbase superuser upsert "$PB_SUPERUSER_EMAIL" "$PB_SUPERUSER_PASSWORD" --dir pb_data >/dev/null)
(cd "$E2E_WORK/pb" && SUPABASE_AUTH_URL=http://localhost:9999 SUPABASE_ANON_KEY="$ANON_KEY" \
  exec ./pocketbase serve --http 127.0.0.1:8090 --dir pb_data > "$E2E_WORK/pocketbase.log" 2>&1) & PIDS+=($!)
wait_http "$PB_URL/api/health" PocketBase
python3 "$E2E/setup_target.py"

log "sb2pb analyze (read-only report)"
export SUPABASE_DB_URL="postgresql://postgres:${DEMO_PW}@localhost:54322/postgres"
(cd "$E2E_WORK" && ./sb2pb analyze --html readiness.html --json readiness.json)

log "sb2pb migrate (non-interactive)"
(cd "$E2E_WORK" && SUPABASE_SECRET_KEY="$SERVICE_KEY" ./sb2pb migrate --yes --allow-blockers --storage http://localhost:8000/storage/v1)

log "Deploying the generated hooks"
cp "$E2E_WORK/pb_hooks/supabase_migration.pb.js" "$E2E_WORK/pb/pb_hooks/"
sleep 3; wait_http "$PB_URL/api/health" PocketBase

log "Verifying behaviour (auth, rules, files, data) and the data type matrix"
python3 "$E2E/verify.py"
python3 "$E2E/verify_types.py"

log "sb2pb rollback"
(cd "$E2E_WORK" && ./sb2pb rollback --yes)
python3 - <<'PY'
import os, requests
pb = os.environ["PB_URL"]
tok = requests.post(f"{pb}/api/collections/_superusers/auth-with-password", json={"identity": os.environ["PB_SUPERUSER_EMAIL"], "password": os.environ["PB_SUPERUSER_PASSWORD"]}).json()["token"]
names = sorted(c["name"] for c in requests.get(f"{pb}/api/collections?perPage=500", headers={"Authorization": tok}).json()["items"] if not c["system"])
users = requests.get(f"{pb}/api/collections/users/records?perPage=1", headers={"Authorization": tok}).json()["totalItems"]
assert names == ["users"] and users == 0, (names, users)
print("rollback ok: only the empty users collection is left")
PY

log "All e2e checks passed"
