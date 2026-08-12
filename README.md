# Echo

A self-hosted music server. Serves a local library, streams from YouTube with a
short-lived cache, and runs as an installable PWA with offline playback.

Design and rationale: [`docs/design.md`](docs/design.md).

**Status: M3.** Authentication (Google + OIDC), the library scanner, and the
library API — browsing with filters, keyset pagination, full-text and fuzzy
search, and metadata corrections. No playback yet.

## Stack

| | |
|---|---|
| Server | Go 1.26, chi + [huma](https://huma.rocks) (OpenAPI 3.1 from Go types) |
| Database | PostgreSQL 18+, pgx/v5, [sqlc](https://sqlc.dev), goose migrations |
| Client | React 19 + Vite, types generated from the server's OpenAPI document |
| Media | ffmpeg, yt-dlp (both invoked as subprocesses) |

PostgreSQL **18** is a hard floor: the schema uses the built-in `uuidv7()`.

## Quick start

```sh
cp .env.example .env       # set ECHO_MUSIC_DIR and at least one sign-in method
make up
curl localhost:8080/api/v1/health
```

Open http://localhost:8080 and sign in. **The first person to sign in becomes
the administrator**; everyone after must be listed in `ECHO_ALLOWED_EMAILS`.

## Development

The server and client run separately; Vite proxies `/api` to the Go server so
requests stay same-origin and the session cookie behaves as it will in
production.

```sh
make dev-up          # Postgres + a local Dex identity provider
make dev-server      # the Go server on the host, wired to Dex
cd web && npm ci && npm run dev                # :5173
```

`make dev-up` brings up [Dex](https://dexidp.io) so the SSO flow works without
registering an OAuth client anywhere. Sign in as `jonathan@example.com` /
`password`. Set `ECHO_DEV_BASE_URL` on both commands to move off port 8080.

The server runs on the host rather than in a container because an OIDC issuer is
compared verbatim, and the same URL has to resolve for both server-side
discovery and the browser redirect — only `localhost` satisfies both.

`go build` works on a fresh checkout with no Node toolchain: the client is only
compiled in under the `embedweb` build tag, which `make build` and the
Dockerfile set.

### Make targets

```
make help              List all targets
make build             Server with the client embedded
make build-server      Server only, no Node required
make generate          Run sqlc and regenerate client API types
make test              Unit tests
make test-integration  Integration tests (needs a Docker daemon)
make lint              go vet in both build configurations, plus gofmt
make migration name=x  Scaffold a new migration
```

## Code generation

Two generated artifacts, both committed, both drift-checked in CI:

| Artifact | Source | Command |
|---|---|---|
| `internal/db/dbgen/` | `internal/db/queries/*.sql` | `make sqlc` |
| `web/src/api/schema.d.ts` | Go handler types → `openapi.yaml` | `make types` |

The second is the reason there is no hand-maintained API type anywhere: change
a handler's signature in Go and the client fails to typecheck.

## Layout

```
cmd/echo/            Entry point: serve | migrate | openapi | version
internal/
  api/               chi + huma wiring, handlers, middleware, static client
  auth/              OIDC providers, sessions, argon2id, admin bootstrap
  blobstore/         Storage for derived data (transcodes, YT cache, art)
  config/            ECHO_-prefixed environment configuration
  db/                Pool, migrations, sqlc queries and generated code
  dbtest/            Postgres container harness for integration tests
  jobs/              SKIP LOCKED work queue with LISTEN/NOTIFY
  library/           Scanner, tag probe, normalisation, filesystem watcher
  version/           Build identity
  webui/             Embedded client (behind the `embedweb` build tag)
web/                 React + Vite client
docs/design.md       Architecture and decision record
```

`blobstore.Store` is an interface with one local-disk implementation. It exists
so that derived data can move to shared object storage if the deployment ever
grows past a single replica — see "Scaling, honestly" in the design doc.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `ECHO_DATABASE_URL` | — | Required |
| `ECHO_ADDR` | `:8080` | |
| `ECHO_LIBRARY_ROOTS` | — | Comma-separated. The **last** root is writable and receives YouTube promotions |
| `ECHO_SCAN_ON_START` | `true` | Queue a full scan at startup |
| `ECHO_SCAN_WORKERS` | `4` | Background job workers |
| `ECHO_CACHE_DIR` | `./cache` | Derived data; disposable |
| `ECHO_YT_CACHE_TTL` | `48h` | Sliding window from last access |
| `ECHO_YT_CACHE_MAX_BYTES` | `5GB` | LRU eviction above this |
| `ECHO_TRANSCODE_CACHE_MAX_BYTES` | `10GB` | LRU eviction above this |
| `ECHO_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `ECHO_BASE_URL` | `http://localhost:8080` | Public URL. OAuth redirect URIs derive from it; cookies are `Secure` when it is https. No trailing slash |
| `ECHO_GOOGLE_CLIENT_ID` / `_SECRET` | — | Enables Sign in with Google. Set both or neither |
| `ECHO_OIDC_ISSUER_URL` | — | Generic OIDC issuer, copied verbatim from the provider's discovery document |
| `ECHO_OIDC_CLIENT_ID` / `_SECRET` | — | Credentials for the generic provider. All three OIDC vars set together |
| `ECHO_OIDC_NAME` | `SSO` | Label on the sign-in button |
| `ECHO_ALLOWED_EMAILS` | — | Comma-separated addresses allowed to **sign up** after the first user |
| `ECHO_LOCAL_AUTH` | `false` | Enable email/password accounts |
| `ECHO_SESSION_TTL` | `720h` | Session lifetime |
| `ECHO_ADMIN_EMAIL` | — | Bootstrap a local admin; requires `ECHO_LOCAL_AUTH=true` and an empty users table |
| `ECHO_ADMIN_PASSWORD` | — | Must be set together with the email |

Library files are mounted read-only and are never modified. Metadata edits are
stored in the database and applied at read time.

## Library API

`GET /tracks` filters by artist, album, genre, and year, keyset-paginated on an
opaque cursor so a page boundary survives the library changing mid-scroll —
which it does constantly while a scan runs.

`GET /search` runs ranked full-text search and widens to trigram similarity when
exact matching finds too little, so `radiohed` still finds Radiohead and `bjork`
reaches `Björk`. Results say which mode produced them.

`PATCH /tracks/{id}` records a metadata correction. Corrections live in
`track_overrides` and are applied at read time through a view — **the audio file
is never modified** — and the search index is rebuilt in the same transaction,
so a corrected title is immediately findable. `DELETE /tracks/{id}/override`
reverts to the file's own tags.

Everything except `/health`, `/auth/providers`, and `/auth/login` requires a
session. Authorisation is default-deny: a new endpoint is private until it is
deliberately added to the public allowlist.

## Library scanning

A scan walks each root and compares size and mtime against what the database
already knows, so a rescan of an unchanged library costs seconds rather than a
re-read. Changed files go to a bounded worker pool that reads tags, extracts
cover art, and reconciles artists and albums by a normalised name — "The
Beatles", "Beatles", and "BJÖRK" all collapse correctly.

**Moves preserve track identity.** A file whose content hash matches a row whose
source file has disappeared is the same track relocated, so reorganising a
library keeps playlists and play history intact. A *copy* is not a move: the
source must actually be gone.

**Deletions are marked, not applied.** A vanished file sets `missing_at` rather
than deleting the row, so an unmounted drive does not destroy playlists. Rows
are purged after a grace period.

A filesystem watcher picks up changes between scans, debounced by three seconds
so a large file being copied is read once, complete. It must run as a
singleton — see "Scaling, honestly" in the design doc.

Test fixtures are real encoded audio committed under
`internal/library/testdata/`, tagged in Go at test time. Nothing in the suite
shells out to ffmpeg, so tests never silently skip.

## Authentication

Sign-in is delegated to an identity provider — Google, a generic OIDC provider
(Authentik, Keycloak, Pocket ID, …), or both — using the authorization-code flow
with PKCE. Discovery runs at startup, so a bad issuer fails the deployment
rather than somebody's first sign-in.

Accounts resolve by provider subject first, then by email. Subject-first means a
user who changes their address at the provider keeps their account;
email-fallback means signing in with a second provider links to the existing
account instead of duplicating it.

**The first person to sign in becomes the administrator.** After that, new
sign-ups must be on `ECHO_ALLOWED_EMAILS` — the list gates sign-up, not sign-in,
so existing users always keep working. The last active administrator cannot be
deleted, demoted, or disabled.

Sessions are opaque 256-bit tokens in an HttpOnly, SameSite=Lax cookie; only
their SHA-256 digest is stored. Cookies rather than bearer tokens because
`<audio src>` cannot send an `Authorization` header, and neither can a service
worker replaying a cached range request. State-changing requests additionally
need the CSRF token from the readable `echo_csrf` cookie, echoed back in
`X-CSRF-Token`.

Local password accounts (`ECHO_LOCAL_AUTH=true`) are off by default and exist
for bootstrapping before an IdP is configured, or as a break-glass account.
Passwords are argon2id (19 MiB, t=2), rehashed on login if parameters are
raised.

### Setting up Google

Create an OAuth client (type "Web application") in the Google Cloud console with
the authorized redirect URI `$ECHO_BASE_URL/auth/google/callback`, then set
`ECHO_GOOGLE_CLIENT_ID` and `ECHO_GOOGLE_CLIENT_SECRET`.

### Setting up a generic OIDC provider

Register a confidential client with redirect URI
`$ECHO_BASE_URL/auth/oidc/callback` and scopes `openid email profile`. Set
`ECHO_OIDC_ISSUER_URL` to the issuer exactly as the provider's discovery
document reports it — a mismatch that is *only* a trailing slash is corrected
automatically with a warning, but anything else fails at startup.
