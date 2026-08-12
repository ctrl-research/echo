# Echo

A self-hosted music server. Serves a local library, streams from YouTube with a
short-lived cache, and runs as an installable PWA with offline playback.

Design and rationale: [`docs/design.md`](docs/design.md).

**Status: M6.** Authentication (Google + OIDC), the library scanner, the library
API, playback, per-user state, and YouTube — search, cached playback, and
promotion into the library.

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
make test              Go unit tests
make test-web          Client tests (player queue logic)
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
| `ECHO_YT_MAX_LIFETIME` | `336h` | Ceiling on how far the sliding TTL can reach |
| `ECHO_YT_COOKIES_FILE` | — | `cookies.txt` for yt-dlp, if your address is challenged |
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

## Streaming

`GET /tracks/{id}/stream` opens the original file and hands it to
`http.ServeContent`, which implements Range, `If-Range`, `If-Modified-Since`,
and multipart ranges correctly. Seeking in a browser is entirely a property of
getting those right.

Originals are served untouched for MP3, M4A, FLAC, OGG, Opus, and WAV — which is
essentially any real library, and transcoding FLAC would throw away the quality
that is the reason to keep it. Anything else (WMA, WavPack, Musepack) is
converted to Opus **into a cache file first**, then served on the same path.
Piping ffmpeg straight to the response would make the track unseekable: the
length is unknown and the stream cannot be rewound. Without ffmpeg installed,
those formats return `415` and the rest of the library is unaffected.

ETags come from the scanner's content hash, so a re-listen is a `304` rather
than a fresh download.

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

## YouTube

`GET /youtube/search` runs a metadata-only yt-dlp query. Playing a result
downloads it to the cache first and then serves it on the ordinary
`ServeContent` path, so seeking works. Proxying the CDN URL instead would leave
the track unseekable — the length is unknown and the stream cannot be rewound —
and those URLs are IP-bound and expire within hours.

Nothing on this path carries ads: `bestaudio` is the raw audio track, and ads
live in the video delivery layer. `--sponsorblock-remove` handles the different
problem of host-read sponsorships baked into the audio itself.

**The cache TTL slides on access**, 48 hours by default, capped at 14 days from
download so one much-played item cannot hold space forever. A sweep every 15
minutes drops expired items, then trims least-recently-used until the cache fits
`ECHO_YT_CACHE_MAX_BYTES`.

**Promoting** copies a cached item into the writable library root, where the
scanner picks it up as an ordinary track. Promoted items are exempt from
eviction — their bytes live under a library root now, not in the disposable
cache. The copy is not a move, so playback continues uninterrupted until the
scan completes.

Downloading from YouTube is contrary to its terms of service. This is a personal
self-hosted tool; the choice is yours to make.

### When it breaks

YouTube breaks extraction every few weeks, so `GET /youtube` reports the yt-dlp
version in use — the first question when search starts failing. Update by
rebuilding the image. If your address is rate-limited or challenged, point
`ECHO_YT_COOKIES_FILE` at an exported `cookies.txt`; residential connections
usually do not need it.

## Shuffle

Shuffle is an **order**, not a dice roll per track. Picking at random each time
plays the same song twice in a row often enough to be irritating, and can play
one track three times before others have played at all.

Enabling shuffle builds a permutation anchored on whatever is playing, so it
never interrupts the current track. Advancing walks that permutation, which
guarantees every track plays once before any plays twice. At the end of a cycle
with repeat-all it reshuffles, keeping the track that just finished out of first
place so the wrap is not a consecutive repeat either. Repeat-one still repeats —
that is a listener asking for it.

## Playlists, favourites, and history

Playlists are private by default and can be shared read-only: a public playlist
is visible to any signed-in user, but only its owner can change it. Entries are
identified by their own id rather than by track, so the same song can appear
twice — and removing one of them removes the right one.

Adding a track that is already in a playlist is refused with `409` unless the
request sets `allowDuplicate`; the client turns that into a confirmation dialog.
A set can repeat a song deliberately, but it should be a decision rather than
the silent result of a mis-click. When a playlist does hold a song twice, the
now-playing highlight follows the **position** rather than the track, so exactly
one row lights up.

Favourites and play history are strictly per-user. The favourite flag on a track
listing reflects the caller, so two people browsing the same library see their
own hearts.

A play counts once it passes **half the track or four minutes, whichever comes
first** — Last.fm's rule, so counts stay comparable if history is ever forwarded
elsewhere. The threshold is checked against the duration the server knows, not
one the client reports, so nobody can inflate their own counts. Plays outlive
their tracks: deleting a file does not erase the fact that it was listened to.

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
