# Security

sb2pb handles database passwords, Supabase secret keys and PocketBase superuser credentials.

## How credentials are handled

- Keys and passwords are read from hidden terminal prompts or environment variables, never from
  command-line flags (flags end up in shell history and process lists). The connection string,
  which contains the database password, can also be given with `--from`; sb2pb warns when it is,
  so prefer the prompt or `SUPABASE_DB_URL`.
- They are kept in memory only. The migration report, the generated hooks and the terminal output
  contain no credentials; the connection string is only ever printed with the password masked.
- The Supabase session runs with `default_transaction_read_only = on`.
- The temporary working copy of the migrated data is created with private permissions and deleted
  when the command ends (unless `--keep-work` is given).

## Reporting a vulnerability

Please do not open a public issue. Use GitHub's private vulnerability reporting
(Security → Report a vulnerability) on this repository. We aim to answer within 3 working days.

## Verifying a release

Release checksums are signed with Sigstore keyless signing from the release workflow:

```sh
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/everlasting18/sb2pb/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
sha256sum -c checksums.txt --ignore-missing
```
