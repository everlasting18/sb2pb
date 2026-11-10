# Changelog

All notable changes to sb2pb are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/). Each release also names the PocketBase release it embeds.

## [Unreleased]

## [0.1.0] - 2026-11-09

First public release. Embeds PocketBase v0.41.1.

### Added
- `sb2pb` / `sb2pb migrate`: interactive wizard and non-interactive mode (`--yes`, environment
  variables) that moves tables, users (bcrypt passwords kept), OAuth identities, Storage files and
  RLS policies into a fresh PocketBase instance, with a safety backup and record-count verification.
- `sb2pb analyze`: read-only readiness report (terminal, HTML, JSON) that flags behaviour changes
  before anything moves, such as files in private buckets following a table's view rule.
- `sb2pb rollback`: restores the pre-migration backup, also when automigrate wrote new
  `pb_migrations` files after the move.
- RLS translation, including membership tables (`EXISTS` / `IN` through a `memberships` table) and
  `WITH CHECK`; anything untranslatable is locked to superusers and listed. A rule only opens for the
  roles app requests run as (`anon`, `authenticated`) and only where they hold the table privilege:
  policies for `service_role`, internal or custom roles open nothing.
- One table keyed by the user id (`profiles`) is merged into `users`; other such tables stay
  collections with the user id as record id. Profile columns named like a sign-in field (`email`,
  `phone`, `verified`…) are kept as `profile_<name>` instead of replacing the value from `auth.users`.
- Storage downloads are verified and retried; a file that cannot be copied completely is listed as an
  error and never stored as a partial file. A download with no data for 60 s is given up; `migrate`
  exits with code 3 when the migration is in place but files are missing.
- Schemas PocketBase cannot hold (table or column names with spaces, dashes or non-ASCII letters, a
  `public.users` table with its own key) are named by `analyze` and stop `migrate` before the target
  is touched.
- Tables without a primary key keep identical rows; reserved column names (`id` outside the key,
  `expand`…) are kept as `<name>_col`; a partitioned table becomes one collection.
- Transaction pooler connection strings (port 6543) are refused with a pointer to the Session pooler.
- Ctrl+C at a hidden prompt leaves the terminal as it was.
- Generated hooks: column `DEFAULT`s, `DEFAULT auth.uid()` columns filled with the signed-in user,
  session exchange for users signed in on Supabase, and route stubs for Postgres functions that say
  whether the function was `SECURITY INVOKER` or `SECURITY DEFINER`.
- "Updated at" triggers (`moddatetime(col)` or a one-line `NEW.col = now()` function) become autodate
  fields that PocketBase sets on every update.
- A version check that accepts a PocketBase server with the same data format as the embedded release.
- macOS, Linux and Windows binaries, Docker image, Homebrew cask, Scoop manifest, signed checksums.

[Unreleased]: https://github.com/everlasting18/sb2pb/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/everlasting18/sb2pb/releases/tag/v0.1.0
