# dbdiagram-cli

Command-line sync for dbdiagram: push DBML, SQL, `schema.rb` or a live database's schema to a diagram, and pull it back.

> **appleby.cloud** · Go CLI · dbdiagram

---

## Overview

`dbdiagram` keeps a diagram in step with the schema it documents:

```bash
dbdiagram auth login                        # paste a team API token (sync scope)
dbdiagram init --source <sourceID> --entry schema.dbml
dbdiagram push --message "Add orders"       # schema.dbml → new diagram version
dbdiagram pull                              # diagram → schema.dbml + schema.dbdiagram (layout)

# straight from a database (read-only session; prefer --tls verify-full)
DB_PASSWORD=… dbdiagram inspect --driver postgres --host db.example.com --user schema_ro \
  --database app --password-env DB_PASSWORD --tls verify-full > schema.dbml
dbdiagram inspect … | dbdiagram push --stdin
dbdiagram push --inspect --driver mysql --host … --user … --database … --password-env DB_PASSWORD
```

Pushing SQL or `schema.rb` works too (`--format mysql|postgres|mssql|schemarb`); the API imports it.

## Role in the Trailblaze ecosystem

Part of the dbdiagram product on appleby.cloud (not Trailblaze): the client for
**dbdiagram-api**'s source push/pull routes, used by people and by CI. The GitHub Action in this
repo runs it on every push. It depends on **go-dbml** for DBML and on Atlas's inspectors for live
databases. Tokens and sources are created in **dbdiagram-web**.

## Tech stack

- Go 1.25, standard library `flag`
- go-dbml v0.1.1 (`atlasrealm.Import`: realm → DBML + warnings)
- Atlas v1.2.2 inspectors (MySQL, MariaDB, Postgres, SQLite)
- go-sql-driver/mysql, lib/pq, modernc.org/sqlite (pure Go — static binaries)
- testcontainers-go for the integration suite

## Getting started

### Prerequisites

- Go 1.25
- A dbdiagram team API token with the `sync` scope, and a source (`kind: cli`) on the diagram
- Docker and a `../dbdiagram-api` checkout, for the integration suite only

### Setup

```bash
dev build                     # → bin/dbdiagram
bin/dbdiagram auth login      # token from a no-echo prompt (or --token / DBDIAGRAM_TOKEN)
bin/dbdiagram auth status     # shows dbd_xxxxxxxx… only — never the token
```

The token is stored at `$(os.UserConfigDir)/dbdiagram/credentials.json` with mode 0600.
`DBDIAGRAM_TOKEN` and `DBDIAGRAM_API` override it (use them in CI).
`.dbdiagram/settings.json` is safe to commit; `.dbdiagram/state.json` is local and gitignored.

The CLI treats a checkout as untrusted: it reads and writes project files only inside the project
directory (`os.Root`), never through a symlink at any path component, never inside
`.git*`/`.dbdiagram`, and only regular files of bounded size. `--out` is the only way to write
elsewhere (`--out -` writes to stdout). It never follows HTTP redirects.

For live databases use `--tls verify-full` (add `--tls-ca` for a private CA); the default
`--tls require` encrypts but does not verify the certificate and warns each time.

## Development

| Command | What it does |
|---|---|
| `dev run -- <args>` | Run the CLI from source |
| `dev build` | Build `bin/dbdiagram` |
| `dev release` | Cross-compile darwin/linux/windows × amd64/arm64 into `dist/` (local only) |
| `dev test` | Unit tests, then the integration suite (Docker) |
| `dev test-unit` | Unit tests only |
| `dev test-integration` | MariaDB 11.4 / Postgres 16 inspection + end-to-end against the real API |
| `dev fmt` | gofmt |
| `dev vet` | go vet (plain and integration builds) |
| `dev check` | fmt + vet + unit tests |
| `dev tidy` | `go mod tidy` keeping Go 1.25 |

## Project structure

```
main.go          entry point (version via -ldflags)
commands/        auth, init, push, pull, inspect, version
client/          push/pull HTTP client and error messages
config/          credentials, settings, state, size limits
introspect/      read-only connections, Atlas inspection → DBML
testenv/         integration containers
scripts/         release.sh (cross-compile + notices), third_party_notices.sh
action.yml       GitHub Action
```

## Deployment

Nothing is deployed. Use it in GitHub Actions — store the token as a repository secret
(`Settings → Secrets and variables → Actions → DBDIAGRAM_TOKEN`) and never inline it:

```yaml
- uses: actions/checkout@v4
- uses: aidenappl/dbdiagram-cli@v0.1.0
  with:
    token: ${{ secrets.DBDIAGRAM_TOKEN }}
    source-id: <sourceID>          # or commit .dbdiagram/settings.json
    entry: db/schema.dbml
```

From a live database (give the CI user `SELECT, REFERENCES` only; the session is read-only anyway).
The `inspect` input is split on whitespace only — no quoting or variable expansion:

```yaml
- uses: aidenappl/dbdiagram-cli@v0.1.0
  env:
    DB_PASSWORD: ${{ secrets.SCHEMA_RO_PASSWORD }}
  with:
    token: ${{ secrets.DBDIAGRAM_TOKEN }}
    source-id: <sourceID>
    inspect: --driver postgres --host db.example.com --user schema_ro --database app --password-env DB_PASSWORD --tls verify-full
```

> ⚠️ Never run this action from a `pull_request_target` workflow that checks out the PR head: the
> pull request's own `settings.json` and schema files would be pushed with your repository's
> token. Use it on `push` to your default branch (or `pull_request`, where secrets are withheld
> from forks).

The action builds the CLI from its own checkout, so the binary matches the ref you pin; it pins
`actions/setup-go` by commit SHA and masks the token. Release
binaries are built locally with `dev release` and published by hand.

## Contributing & further reading

Licensed Apache-2.0 — [LICENSE](LICENSE), [NOTICE](NOTICE). Third-party licenses for everything
compiled into the binary are in [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES) (regenerate with
`scripts/third_party_notices.sh`); release archives include all three.

- [AGENTS.md](AGENTS.md) — conventions, the push/pull contract, connection security, verification
- dbdiagram-api `AGENTS.md` — "API tokens" and "Sources & sync" (the server contract)
- go-dbml `AGENTS.md` — the DBML compiler, emitter and `atlasrealm`
