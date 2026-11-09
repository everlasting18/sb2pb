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
  `WITH CHECK`; anything untranslatable is locked to superusers and listed.
- Generated hooks: column `DEFAULT`s, `DEFAULT auth.uid()` columns filled with the signed-in user,
  session exchange for users signed in on Supabase, and route stubs for Postgres functions that say
  whether the function was `SECURITY INVOKER` or `SECURITY DEFINER`.
- "Updated at" triggers (`moddatetime(col)` or a one-line `NEW.col = now()` function) become autodate
  fields that PocketBase sets on every update.
- A version check that accepts a PocketBase server with the same data format as the embedded release.
- macOS, Linux and Windows binaries, Docker image, Homebrew cask, Scoop manifest, signed checksums.

[Unreleased]: https://github.com/pocketbasecloud/sb2pb/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/pocketbasecloud/sb2pb/releases/tag/v0.1.0
