# Contributing

## Layout

| Path | |
|---|---|
| `cmd/sb2pb` | entry point |
| `internal/app/cli.go`, `prompt.go` | commands, flags, terminal prompts |
| `internal/app/source.go` | Supabase connection, read-only sessions, keys |
| `internal/app/analyze.go`, `types.go` | catalog analysis and the Postgres → PocketBase type mapping |
| `internal/app/rules.go` | RLS policy → API rule translation |
| `internal/app/readiness.go`, `analyzecmd.go`, `templates.go` | readiness score, terminal and HTML report |
| `internal/app/build.go`, `views.go` | building collections, records, files, rules, views and hooks |
| `internal/app/pocketbase.go`, `migrate.go`, `rollback.go` | PocketBase API, backup/restore flow |
| `e2e/` | end-to-end suite against a real self-hosted Supabase stack |

## Tests

```sh
make test             # gofmt, golangci-lint, go vet, unit tests with -race
make e2e              # needs Docker (compose v2), Go and Python 3: pip install -r e2e/requirements.txt
KEEP=1 e2e/run.sh     # leave the stack running to poke at it (Supabase on :54322/:8000, PocketBase on :8090)
make build            # bin/sb2pb
```

`e2e/run.sh` starts Supabase (Postgres, Auth, Storage, REST, gateway) and a mock OIDC provider,
seeds users of every kind (bcrypt, argon2, MFA, anonymous, phone, OAuth), files and 150k rows,
runs `sb2pb analyze` and `sb2pb migrate --yes` into a fresh PocketBase, then checks behaviour
(`e2e/verify.py`: logins with original passwords, OAuth linking, token exchange, rules, files),
every value of every column type (`e2e/verify_types.py`) and `sb2pb rollback`.

## Upgrading PocketBase

sb2pb embeds the PocketBase core and builds the data with it, so the server must have the same
data format (the same system migrations); see `checkVersion` in `pocketbase.go`.

1. `go get ganigeorgiev.com/pocketbase/pocketbase@<version>` (and the same in `e2e/pocketbase`).
2. Update `pbVersion` in `internal/app/cli.go` (a unit test fails until they match) and the
   provider list in `readiness.go` if PocketBase added OAuth2 providers.
3. Run the e2e suite, then release.

## Pull requests

1. Open an issue first for anything bigger than a small fix, so we can agree on the approach.
2. Branch from `main`, keep the change focused, and add a unit test for new behaviour.
   Changes to migration, rules, types or files also need `make e2e`.
3. Add a line under "Unreleased" in `CHANGELOG.md` for anything a user would notice.
4. CI must be green; a maintainer reviews and squash-merges.

### Commit messages

We use [Conventional Commits](https://www.conventionalcommits.org): `type(scope): summary` in
the imperative, under 72 characters, with the reason in the body when it is not obvious.

```
feat(rules): translate membership-table RLS into @collection joins
fix(rollback): mark automigrate files as applied so PocketBase starts
docs: explain behaviour changes for private buckets
test(e2e): cover OAuth-only users
deps: bump PocketBase to v0.41.1
```

Types: `feat` (new behaviour), `fix` (bug), `docs`, `test`, `refactor` (no behaviour change),
`perf`, `ci`, `deps`, `chore`. Add `!` for a breaking change (`feat!: ...`) and explain it in the body.

## Releasing

Move the "Unreleased" entries in `CHANGELOG.md` under the new version, then tag `vX.Y.Z` and push
the tag (`v0.2.0` for new features, `v0.1.1` for fixes only). The release workflow builds binaries for macOS, Linux and Windows,
signs the checksums, publishes the Docker image and updates the Homebrew tap and Scoop bucket.
It needs a `HOMEBREW_TAP_TOKEN` secret with write access to `pocketbasecloud/homebrew-tap` and
`pocketbasecloud/scoop-bucket`.
