# End-to-end suite

`./run.sh` from the repository root or this folder. See [CONTRIBUTING.md](../CONTRIBUTING.md#tests).

- `supabase/` self-hosted Supabase stack; `demo.env` holds Supabase's public demo keys (test only)
- `fixtures/` schema, a table with every Postgres type, and the seeding script
- `pocketbase/` PocketBase v0.41.1 test server (with JS hooks), a separate Go module
- `mock_idp.py` minimal OIDC provider for the OAuth checks
- `verify.py`, `verify_types.py` checks run after the migration; results land in `.work/`
