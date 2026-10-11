# AGENTS.md — dbdiagram-cli

The working guide for this repo. Read it end to end before changing anything; update it in the
same change as any code that alters what it describes.

## 1. What this repo is

`dbdiagram` is the command-line client for dbdiagram (appleby.cloud) — plan Task 4.2,
"Recommendation 3: sync engine — CLI push first". It keeps a diagram in step with a schema that
lives somewhere else:

- **push**: sends a DBML / SQL (MySQL, Postgres, SQL Server) / Rails `schema.rb` file — or a live
  database inspection — to a diagram **source** (`PUT /v1/sources/{id}/push` on dbdiagram-api).
- **pull**: writes the diagram's DBML to the entry file and its canvas layout to a companion
  `<entry>.dbdiagram` file (`GET /v1/sources/{id}/pull`).
- **inspect**: reads a live MySQL / MariaDB / Postgres / SQLite schema with Atlas's inspectors and
  prints DBML (go-dbml's `atlasrealm.Import`).
- A composite **GitHub Action** (`action.yml`) that builds the CLI and runs `push`.

It owns: the on-disk formats (`credentials.json`, `.dbdiagram/settings.json`,
`.dbdiagram/state.json`, `<entry>.dbdiagram`), the client side of the push/pull contract, and the
safety rules for connecting to customer databases. It does **not** own: the API contract (dbdiagram-api
owns it), DBML parsing/emitting (go-dbml), token minting (the web app / API), or any server-side
sync (the GitHub App and the DB sync worker are Tasks 4.3 / 5.1 in dbdiagram-api).

## 2. Stack & dependencies

- Go **1.25** (`go 1.25.0` directive — keep it; several dependencies' newest versions need 1.26,
  so they are pinned below that: `golang.org/x/term v0.45.0`, `modernc.org/sqlite v1.59.0`,
  `ariga.io/atlas v1.2.2`). Run `GOTOOLCHAIN=local go mod tidy` (`dev tidy`) so tidy never raises it.
- Standard library `flag` for commands — no CLI framework.
- `github.com/aidenappl/go-dbml v0.1.1` — `importer/atlasrealm.Import` (model + DBML + warnings), and
  `dbml.ParseFile` in tests.
- `ariga.io/atlas v1.2.2` (Apache-2.0) — `sql/mysql`, `sql/postgres`, `sql/sqlite` inspectors only.
- Drivers: `github.com/go-sql-driver/mysql v1.9.3`, `github.com/lib/pq v1.12.3`,
  `modernc.org/sqlite v1.59.0` (pure Go, so every target cross-compiles with `CGO_ENABLED=0`).
- Tests: standard `testing` (table-driven, no testify), `httptest`, and
  `testcontainers-go` (+ `modules/mariadb`, `modules/postgres`) behind `//go:build integration`.
- No telemetry: a CLI on users' machines never phones home. `--verbose` is the only diagnostic.
- License: **Apache-2.0** (`LICENSE`, `NOTICE`). `THIRD_PARTY_NOTICES` is generated from the
  shipped module graph by `scripts/third_party_notices.sh` (license + NOTICE files verbatim, and
  the Source Code Form location for the MPL-2.0 modules go-sql-driver/mysql and hashicorp/hcl/v2).
  Regenerate it after any dependency change; `release.sh` refuses stale notices.

## 3. Project structure

```
main.go                     entry point; `var version` set by -ldflags "-X main.version=…"
commands/                   one file per command (stdlib flag, ContinueOnError)
  root.go                   Env (injectable stdin/stdout/env/HTTP for tests), dispatch, exit codes,
                            API/token resolution (newClient), ValidateAPI
  auth.go                   auth login | logout | status
  init.go                   init → .dbdiagram/settings.json + .dbdiagram/.gitignore
  push.go                   push (entry file | --stdin | --inspect), git sha/ref resolution
  pull.go                   pull, dirty-file protection, layout companion
  inspect.go                inspect, the shared inspect flag set, password prompt
  version.go, git.go        version string; read-only `git rev-parse` helper
  commands_test.go          parsing, auth/init/push/pull against a fake API (httptest)
  security_test.go          regression tests for every security-review repro (symlinks, metadata
                            entries, special files, redirects, API pairing, password-env)
  e2e_integration_test.go   real dbdiagram-api + MariaDB end to end (integration tag)
client/                     HTTP client for push/pull; APIError → human messages; one 429 retry
config/                     credentials (0600, dir 0700), settings, state, size limits, layout path;
  project.go                os.Root-confined project IO (ReadRegular, WriteFile, WriteExplicit, RelInside)
introspect/                 structured-flag connections, read-only sessions, Atlas inspection,
                            MySQL FK recovery, DBML via atlasrealm.Import
testenv/                    (integration tag) MariaDB 11.4 / Postgres 16 containers on tmpfs
scripts/release.sh          cross-compile 6 targets into dist/ (each with LICENSE, NOTICE,
                            THIRD_PARTY_NOTICES) + SHA256SUMS (local only; rm -rf's dist/)
scripts/third_party_notices.sh  regenerate THIRD_PARTY_NOTICES from the module graph
LICENSE, NOTICE, THIRD_PARTY_NOTICES
action.yml                  composite GitHub Action (build from action path → push)
Devfile.yaml                dev CLI commands
```

## 4. Running, building & testing

| Command | What it does |
|---|---|
| `dev run -- <args>` | `go run .` with arguments |
| `dev build` | `bin/dbdiagram` for this machine, version from `git describe` |
| `dev release` | `scripts/release.sh`: darwin/linux/windows × amd64/arm64 into `dist/` (+ tarballs, `SHA256SUMS`); `VERSION=v0.1.0 dev release` to name it. **Never uploads anything.** |
| `dev test` | unit tests, then the integration suite |
| `dev test-unit` | `go test ./...` (no Docker) |
| `dev test-integration` | `go test -tags=integration -count=1 -p 1 -timeout 20m -v ./...` |
| `dev fmt` / `dev vet` / `dev check` | gofmt; vet of the plain **and** integration builds; both + unit tests |
| `dev tidy` | `GOTOOLCHAIN=local go mod tidy` (then `scripts/third_party_notices.sh` if the graph changed) |

Prerequisites:
- Go 1.25.
- Integration suite: Docker (pulls `mariadb:11.4`, `postgres:16`; data dirs are tmpfs, nothing is
  written to Docker volumes) **and** a checkout of dbdiagram-api at `../dbdiagram-api` (or
  `DBDIAGRAM_API_DIR`). The end-to-end test builds the API into a temp dir and runs it with
  `DBD_ENVIRONMENT=development` against its own container; it **skips with the reason** when the API
  repo is missing, does not build, or never becomes healthy.
- No `.env`. Nothing here needs secrets.

## 5. How code is written here

- Flat layout (no `cmd/`, `internal/`, `pkg/`). Packages by concern: `commands`, `client`,
  `config`, `introspect`, `testenv`.
- Commands take an `*Env` and return `error`; `Run` maps errors to exit codes: `0` ok / help,
  `2` usage (bad flag, missing required flag, unknown command — usage already printed), `1`
  everything else (`error: …` on stderr). Commands never call `os.Exit` or touch `os.*` streams
  directly — tests run them in-process.
- Flags: `flag.NewFlagSet(…, ContinueOnError)`; positional arguments are refused. Shared flag sets
  (`apiFlags`, `inspectFlags`) are registered on each command that takes them (`push --inspect`
  reuses every inspect flag).
- **Every project file read or write goes through `config.Project` (an `*os.Root` on the project
  directory)** — never `os.ReadFile`/`os.WriteFile` on a joined path. Files named on the command
  line (`--out`) go through `config.WriteExplicit` (a root on their own directory). Credentials use
  `writeFileAtomic` (temp file created with its final mode, then rename — no chmod after).
- API errors: `client.APIError` carries status, `error_code`, server message and `Retry-After`;
  `Error()` is the user-facing explanation. Add new codes there, with a case in `TestErrorMapping`.
- Constants `SCREAMING_SNAKE_CASE`; no `any` where a concrete type exists.
- Tests: table-driven, `testing` only. A behaviour that depends on a real database is proven in an
  `*_integration_test.go` against a container, not with a mock.

## 6. Domain & architecture

### Files

| File | Where | Contents | Mode |
|---|---|---|---|
| `credentials.json` | `os.UserConfigDir()/dbdiagram/` (or `$DBDIAGRAM_CONFIG_DIR`) | `{"token": "dbd_…", "api": "…"}` | **0600** (temp file created 0600, renamed; no post-rename chmod), dir forced to **0700** on every save; `auth status` warns when the file is group/world-readable or the dir group/world-writable; a symlinked credentials file is refused |
| `.dbdiagram/settings.json` | project root (found by walking up from cwd) | `{"source_id", "entry", "format"}` — committable, never a secret | 0644 |
| `.dbdiagram/.gitignore` | written by `init` | ignores `state.json` | 0644 |
| `.dbdiagram/state.json` | project root, **local** | `{source_id, version, file, content_sha256, updated_at}` — what the working copy last matched on the server | 0644 |
| `<entry>.dbdiagram` | next to the entry (`schema.dbml` → `schema.dbdiagram`) | the diagram's layout document, pretty JSON (`{}` when none) | 0644 |

`entry` must be relative, contain no control characters, stay inside the root, and have **no path component starting with
`.git`** (`.git/`, `.github/`, `.gitmodules`…, case-insensitive) or equal to `.dbdiagram`, and must
not end in `.dbdiagram` (on Windows, trailing dots/spaces are ignored for this check and any
`~<digit>` 8.3 short-name component is refused); `source_id` must match `^[A-Za-z0-9-]{1,64}$` (it is interpolated into the
URL path, and is path-escaped anyway).

### Project file confinement (security review, Task 4.2)

`settings.json` is committed, so it — and any symlink committed next to it — is attacker-controlled
input for whoever runs `push`/`pull` in a checkout. The review reproduced pull creating
`~/.zshenv` through a committed `home -> ~` symlink, push uploading `~/.config/secret.txt` through a
symlinked entry, and push reading 1.26 GB from an entry symlinked to `/dev/zero`. Now
(`config/project.go`):

- Every read/write of the entry, the layout companion, `settings.json`, `state.json` and
  `.gitignore` goes through `os.OpenRoot(projectRoot)`: no path — `..`, an absolute path, or a
  symlinked directory (including a symlinked `.dbdiagram/`) — can resolve outside the project.
- **No component may be a symlink** — every directory component is `Lstat`ed through the root
  before an open or write (and again after `MkdirAll`), and the final component too, even when
  the link points inside the project. Round 3 of the review reproduced `g -> .git` + entry
  `g/config` (push uploaded `.git/config` with a checkout token), `g/hooks/post-checkout` (pull
  wrote a git hook), and `m -> .dbdiagram` (pull overwrote `state.json`); all are refused now.
  On Windows the real path is additionally resolved (`EvalSymlinks`, which expands 8.3 short
  names) and refused if any real component is `.git*` or `.dbdiagram`.
- For reads and writes alike; reads require a regular file (FIFOs, devices, directories refused), check the size
  from `Lstat`, re-check the opened file is the same regular file (`os.SameFile`), and read through
  `io.LimitReader(max+1)` (entry: the format's limit; settings/state 64 KiB; pull's dirty check
  4 MiB).
- Writes: parents created with `Root.MkdirAll`, a temp file created `O_EXCL` inside the root, then
  `Root.Rename` (on any failure the temp file is removed through the root, never by raw path;
  credentials use the same path on their own directory); an existing symlink or non-regular destination is refused (`--force` does not
  override this).
- `push --entry X` must resolve inside the project (`filepath.Rel`, rejecting `..` and
  `../…`); use `--stdin` for anything else.
- `--out` (pull, inspect; `config.OutTarget`): `-` writes to stdout (pull: DBML only — no layout,
  no state). A **relative** path inside the current directory is confined like an entry (root on
  cwd, every component checked, no `.git*`/`.dbdiagram`). An absolute path, or one leaving cwd, is the
  explicit escape hatch **because the user typed it**: written through a root on its own directory,
  never through a final symlink. Never a `*.dbdiagram` name, never control characters. Nothing read
  from `settings.json` can reach it.
- `init` writes `.dbdiagram/.gitignore` first and `settings.json` last, so a refusal never leaves a
  half-initialised project.
- **Terminal safety:** every printed path is `%q`-quoted; server text (diagnostics, run fields),
  inspection warnings and every error line pass through `config.SafeText`, which escapes C0/C1
  control characters, so a hostile `settings.json`, database or API response cannot emit terminal
  escape sequences.
- **Hard links (accepted, local-attacker only):** a hard link inside the checkout to a file
  elsewhere is a regular file and would be read/written. Git never creates hard links, so a cloned
  repository cannot carry one; only someone who can already write to your filesystem as you can
  plant it.

### Credentials resolution

Token: `$DBDIAGRAM_TOKEN` > `credentials.json`. API origin: `--api` > `$DBDIAGRAM_API` > the stored
login's `api` **only when the token is the stored one** (an env token never goes to an origin some
earlier login wrote to disk) > `https://dbdiagram-api.appleby.cloud`. The origin must be `https://` (plain `http`
only for `localhost` / `127.0.0.1` / `::1`) with no path, query or userinfo — a token never crosses a
network in clear text. **Redirects are never followed** (`CheckRedirect` →
`http.ErrUseLastResponse`): a 3xx is reported as an error naming the Location, so the token is never
replayed to another host. Tokens must match `^dbd_[A-Za-z0-9_-]{43}$`. Only the 12-character prefix
(`dbd_` + 8, the API's own display prefix) is ever printed. `auth login` without `--token` reads the
token from a no-echo prompt (terminal) or stdin, keeping it out of shell history.

### Push / pull contract (owned by dbdiagram-api — its AGENTS.md "API tokens", "Sources & sync")

- `PUT /v1/sources/{id}/push`, `Authorization: Bearer dbd_…`, JSON
  `{"dbml": <content>, "format": dbml|mysql|postgres|mssql|schemarb, "sha", "ref", "message"}`.
  Answer `{run, diagram, compile}`; `run.status` `succeeded` or `skipped` (identical content).
- `GET /v1/sources/{id}/pull` → `{source, diagram: {id, name, version, dbml, updated_at}, layout}`.
- The token needs the **`sync`** scope and acts as its team. Push has **no layout field**: layout
  only flows server → disk.
- Client-side checks before anything is sent: DBML ≤ 1 MiB, SQL / schema.rb ≤ 4 MiB
  (`config.CheckSize`; stdin is read through a `LimitReader` of limit+1); empty content refused;
  `sha` `^[A-Za-z0-9._-]{1,64}$`; `ref` ≤ 255 printable characters. The server additionally caps the
  DBML an import generates at 1 MiB (413).
- `--sha auto` / `--ref auto` (defaults): `git rev-parse HEAD` / `--abbrev-ref HEAD` in the project
  root, falling back to `$GITHUB_SHA` and `$GITHUB_HEAD_REF` / `$GITHUB_REF_NAME` (detached CI
  checkouts); invalid auto values are dropped, invalid explicit values refused; `""` sends none.

Error mapping (`client/errors.go`):

| Status / code | Message |
|---|---|
| 3xx | `the API answered a redirect (…); it is not followed` — check `--api` |
| 400 | `the API rejected the request: <server message>` |
| 401 | token missing/invalid/revoked/expired → `dbdiagram auth login` |
| 403 / 4003 | token lacks the `sync` scope (or role below editor) |
| 403 other | `forbidden (<code>): …` |
| 404 | source not found / not this token's team (the API answers the same 404 for both) |
| 409 | conflict + server message (paused source → resume in the web app; archived team/diagram) |
| 413 | too large + the limits |
| 422 / 4221 | `import failed: …` |
| 429 | **retried once automatically** after `Retry-After` (seconds or HTTP date; 1s when absent; no retry when > 2 min); a second 429 is reported with the limits (push+pull 30/min per token or per signed-in user, push 120/hour per team; SQL/schema.rb imports also wait for a compiler slot → 503) |
| 503 | busy + `Retry-After` (not retried) |
| 5xx | `server error (<status>)` |

### Pull and local changes

Pull refuses to overwrite the destination when it exists, differs from the server's DBML, **and**
its hash differs from `state.json` (same source, same file) — i.e. it was edited since the last
pull **or push** (a successful push of the entry records state too, so a pushed file is clean).
A file never pulled here that differs from the server is also refused. `--force` overwrites. The
destination is the entry; when the entry's format is not `dbml` (the server stores DBML, not SQL),
`--out FILE` is required. The layout companion is always overwritten (the server owns layout).

### Inspect — connecting to customer databases safely

- **Structured flags only.** There is no `--dsn` and no `--password` flag. The password comes from
  `--password-env VAR` or a no-echo terminal prompt (no prompt when stdin is not a terminal:
  empty password). `--password-env DBDIAGRAM_TOKEN` is refused (it would hand the API token to
  whatever `--host` names). `--host` must match `^[A-Za-z0-9._:%-]{1,253}$`, so nothing DSN- or URL-shaped
  passes; SQLite paths may not contain `?#%`.
- **MySQL / MariaDB** (`introspect.MySQLConfig`): `mysql.Config` built field by field —
  `AllowAllFiles=false` (no `LOAD DATA LOCAL INFILE`; no files or readers are ever registered),
  `MultiStatements=false`, `InterpolateParams=false`, no cleartext/old passwords, no plaintext
  fallback, `Params=nil`.
- **Postgres** (`introspect.PostgresConfig`): a `pq.Config` struct built field by field and passed to
  `pq.NewConnectorConfig` — no connection string is parsed and lib/pq's environment handling
  (`PGHOST`, `PGSSLMODE`, `PGOPTIONS`, `PGSERVICE`, `PGPASSFILE`, …) is never consulted (that lives in
  `pq.NewConfig`, which is not called; the integration test sets hostile `PGSSLMODE`/`PGOPTIONS` and
  proves they are ignored). lib/pq may still read libpq's standard `~/.postgresql/root.crt` (CA for
  verify-full without `--tls-ca`) and `~/.postgresql/postgresql.crt/.key` (client cert), and
  `~/.pgpass` only when the password is empty. `application_name=dbdiagram-cli`.
- **TLS** — **recommend `--tls verify-full`** (CA + hostname; `--tls-ca FILE` for a private CA).
  The default `require` encrypts without checking the certificate (libpq semantics; MariaDB's
  auto-generated certificate is self-signed) and prints a one-line warning saying so; `disable` is
  plaintext. A plaintext server is refused by default.
- **Read-only, time-limited sessions**, set on *every* connection by a `driver.Connector` wrapper
  (`sessionConnector`) so a reconnect can never skip them: MySQL/MariaDB
  `SET SESSION TRANSACTION READ ONLY` + `max_execution_time` (MySQL, ms) or `max_statement_time`
  (MariaDB, s) — the one the driver flag names first, the other on error 1193, so `--driver mysql`
  against MariaDB still gets a timeout; Postgres startup parameters `default_transaction_read_only=on`
  + `statement_timeout`, plus `SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY`; SQLite
  `mode=ro` + `query_only(1)`. `--timeout` (default 30s, **minimum 1s** — MariaDB's limit is in seconds and 0 would disable it) per statement; 10s connect timeout; the
  whole inspection is capped at 5 minutes. Pool of 2 connections.
- Atlas mode `InspectSchemas | InspectTables | InspectTypes` (no views, functions, triggers).
  Schemas: MySQL = `--database`; Postgres = `--schema` list (default all non-system schemas);
  SQLite = `main`. The default schema (unqualified in DBML) is the MySQL database / `public` / `main`.
- **Realm → DBML** is go-dbml v0.1.1's `atlasrealm.Import(realm, Options{DefaultSchema, Dialect})`
  (`introspect.RealmDBML`): `Dialect` is `importer.MySQL` for mysql/mariadb, `importer.Postgres` for
  postgres, empty (auto-detected) for sqlite. It returns the model, canonical DBML and warnings.
  Upstream now handles what this repo used to work around in `sanitize.go` (removed): MySQL
  backtick identifiers in CHECK / default / generated / index expressions are normalised per
  dialect, inline MySQL ENUMs get per-column names (`<table>_<column>_enum`), and a FK to a table
  outside the inspected schemas (`--schema billing` → `public.orgs`) is dropped with a warning. The
  integration suite still pins all three (every FK → Ref, two distinct enums, `age >= 0` present).
- **CLI-side fix kept — MariaDB hides `REFERENTIAL_CONSTRAINTS` / `TABLE_CONSTRAINTS` from a
  SELECT-only user**, and Atlas reads FKs by joining it, so the recommended read-only account
  silently lost *every* FK. `recoverMySQLFKs` rebuilds missing FKs from `KEY_COLUMN_USAGE` (still
  visible) and warns; their ON DELETE/UPDATE actions are unknown and omitted. `GRANT SELECT,
  REFERENCES` makes Atlas read them in full (tested both ways).
- All warnings (CLI and `atlasrealm.Import`) print to stderr as `warning: …` through
  `config.SafeText`; DBML goes to stdout (or `--out`), so `inspect | push --stdin` stays clean.

### Exit codes

`0` success / help · `1` failure · `2` usage error.

## 7. Ecosystem & related repos

- **dbdiagram-api** — the only server this talks to (`/v1/sources/{id}/push|pull`). Its AGENTS.md
  "API tokens" and "Sources & sync" are the contract; a change there must be mirrored in `client/`,
  the error table above, and the fake API in `commands/commands_test.go`. Tokens are created
  in the web app (`POST /v1/teams/{teamID}/tokens`, admin, scope `sync`); sources under a diagram
  (`kind: cli`).
- **go-dbml** (`v0.1.1`) — `atlasrealm.Import`, parsing in tests.
- **dbdiagram-web** — where users create tokens and sources, resume paused sources, and see pushes.
- **dbdiagram-mcp** (future) — not related to the CLI.
- Atlas (ariga.io/atlas, Apache-2.0) — inspectors only; no Atlas CLI, no migrations.

## 8. Operations

Not a deployed service. Distribution:

- **GitHub Action** (`action.yml`): `uses: aidenappl/dbdiagram-cli@<ref>` sets up Go
  (`actions/setup-go` **pinned by commit**, `40f1582b2485089dde7abd97c1529aa768e1baff` = v5.6.0, the
  v5 tag when verified with `gh api repos/actions/setup-go/git/ref/tags/v5.6.0`) and builds the
  CLI from the action's own checkout (`go build -C $GITHUB_ACTION_PATH`, version = the ref), so the
  binary always matches the pinned ref — no download step. Inputs: `token` (required; from a
  secret), `source-id`, `entry`, `format`, `inspect` (inspect flags as one string; the password via
  `--password-env` + `env:` on the step), `message` (default: head commit subject), `api`,
  `working-directory`, `go-version`. Inputs reach the script only through `env`, never `${{ }}` in
  shell; the token is `::add-mask::`ed; `inspect` is split on whitespace only (`read -a`: no
  quoting, globbing or expansion — values cannot contain spaces) and may not mention
  `DBDIAGRAM_TOKEN`. Never run it on `pull_request_target` with a PR-head checkout (the PR's
  settings.json and files would be pushed with the base repo's token). No workflow files are
  shipped to other repos.
- **Binaries**: `dev release` builds six statically linked targets locally, each archive with
  `LICENSE`, `NOTICE` and `THIRD_PARTY_NOTICES` (the build fails if the notices are stale). Publishing a GitHub
  release or tag is Aiden's manual step; there is no goreleaser config and nothing here pushes.
- `go install github.com/aidenappl/dbdiagram-cli@vX` installs it as `dbdiagram-cli`
  (module directory name); `version` falls back to the module version from build info.
- Diagnostics: `--verbose` logs `METHOD path → status in duration (request id …)` per request —
  never headers, bodies or the token. The request id matches dbdiagram-api's Monitor events.

Common failure modes: `401` after a token revoke/expiry (re-login); `403/4003` token created
without `sync`; `409` source paused in the web app; `warning: N foreign key(s) were read from
KEY_COLUMN_USAGE…` (grant REFERENCES); `tls require` against a server without TLS (use
`--tls disable` only on a trusted network).

## 9. Rules & guardrails

- Never add a `--password` or `--dsn` flag, never accept a DSN from the user, never turn on
  `AllowAllFiles`, `MultiStatements`, `InterpolateParams` or register LOAD DATA files/readers.
- Never weaken the read-only session or the statement timeout; any new driver gets both, applied
  per connection.
- Never read or write a project file except through `config.Project` (`os.Root`); never follow a
  final symlink; never read a project file unbounded or without requiring a regular file; never
  let a value from `settings.json` choose a path outside the project (`--out` on the command line
  is the only escape hatch). Entries in `.git*`/`.dbdiagram` or named `*.dbdiagram` stay refused.
- Never weaken the per-component symlink check or the metadata-component rule (including the
  Windows real-path / short-name checks); never print a path or server string without `%q` or
  `config.SafeText`.
- Never follow HTTP redirects with the token; never pair a stored API origin with an env token.
- Never chmod by path after a rename; keep the config dir 0700.
- Never parse a connection string for Postgres or let `PG*` environment reach the connection
  (build `pq.Config`, use `NewConnectorConfig`); keep `--timeout` ≥ 1s.
- Keep `actions/setup-go` pinned by commit SHA in `action.yml`; keep the token masked.
- Keep `THIRD_PARTY_NOTICES` in step with the module graph.
- Never print, log or write the token anywhere but `credentials.json` (0600). `--verbose` stays
  header-free. Settings files never carry secrets.
- Never allow a plain-`http` API origin that is not loopback.
- Keep the client-side size limits equal to the API's; when the API changes them, change
  `config` and this file.
- Keep `go 1.25.0`; pin dependencies below versions that need Go 1.26.
- `CGO_ENABLED=0` must keep building every release target (no cgo SQLite).
- Do not commit `dist/`, `bin/`, or a project's `.dbdiagram/state.json`.

## 10. Verification

All of these must pass before work is done:

```bash
gofmt -l .                                  # prints nothing
go vet ./... && go vet -tags=integration ./...
go build ./...
go test ./...
go test -tags=integration -count=1 -p 1 -timeout 20m ./...   # Docker + ../dbdiagram-api
# release.sh rm -rf's dist/: run it in a scratch copy
rsync -a --exclude dist ./ "$TMPDIR/rel" && (cd "$TMPDIR/rel" && VERSION=v0.0.0-check scripts/release.sh)
```

The integration suite proves the plan's acceptance criteria: inspecting MariaDB 11.4 (and
Postgres 16) seeded with single, composite and duplicate-target FKs, a composite PK, enums,
unique/composite indexes, checks and comments yields DBML that compiles with go-dbml **with every FK
as a Ref** (compared against `information_schema`), sessions are read-only with the timeout set,
and a push → pull round trip against the real API is byte-identical. `commands/security_test.go`
holds one regression test per security-review repro (rounds 2 and 3).

## 11. Keeping this file updated

Any change to structure, dependencies, commands, flags, file formats, the push/pull contract, error
mapping, connection security, or the GitHub Action updates this file — and `README.md` when setup,
commands or the repo's role change — **in the same change**. Stale docs mislead every agent that
reads them next.
