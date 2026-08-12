# Echo — Design

A self-hosted music server: serves a local library, streams from YouTube with a
short-lived cache, and runs as an installable PWA with offline playback.

## Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Backend | Go — chi + huma | Best-in-class range streaming, cheap concurrency for scan/transcode |
| Database | **PostgreSQL 18+**, pgx/v5 + sqlc + goose | Real concurrency, `SKIP LOCKED` queue, trigram search, scale-out path |
| Client | React + Vite PWA | Single client; no third-party apps targeted |
| Types | huma → OpenAPI 3.1 → `openapi-typescript` | Closes Go's type-sharing gap without hand-maintained duplicates |
| Library files | **Never mutated.** Edits live in `track_overrides` | Sidesteps Go's weak tag-writing libs; a scanner bug can't corrupt the collection |
| Users | Multi-user from day one | Shared library, per-user playlists/history/favorites |
| Sign-in | Google OAuth + generic OIDC; optional local passwords | Identity lives in an existing IdP rather than in Echo |
| Deploy | Docker Compose in-repo; homelab k8s by hand | No charts shipped |
| Subsonic API | Deferred, schema stays compatible | Escape hatch if iOS PWA background audio disappoints |

### PostgreSQL

Postgres is the only datastore. Three parts of the design lean on it directly:

- **Concurrent writes.** The scanner writes from many goroutines while
  playback reads continue uninterrupted. MVCC means a full library scan never
  blocks a listener.
- **The job queue.** `SELECT … FOR UPDATE SKIP LOCKED` is the canonical
  multi-worker claim pattern, and `LISTEN`/`NOTIFY` wakes workers instantly
  instead of polling. No Redis, no separate broker.
- **Search.** `tsvector` for ranked full-text plus `pg_trgm` for fuzzy and
  substring matching — see [Search](#search). Trigram similarity matters more
  for music than it looks: people search "beatles" for "The Beatles" and
  misspell artist names constantly.

**Toolchain:** `pgx/v5` natively rather than through `database/sql`, `sqlc` to
generate typed Go from hand-written SQL, `goose` for migrations. Postgres
features are used freely — enums, `jsonb`, generated columns, partial indexes,
advisory locks — with no portability layer and no second dialect to support.
Dev and CI run Postgres via compose; integration tests use testcontainers.

**Operational cost.** The service is not self-contained: it needs a database
alongside it, and backups are `pg_dump` or WAL archiving. In k8s that's
near-free — you likely run Postgres already, or CloudNativePG makes it trivial.
In compose it's one more service and a healthcheck.

**Version floor: PostgreSQL 18.** `uuidv7()` became a built-in function in 18;
on older versions it needs the `pg_uuidv7` extension or Go-side generation.
Since this is greenfield and 18 is well past release, the floor is simpler than
the workaround.

`uuidv7` primary keys throughout: time-ordered so index locality stays good,
no coordination needed, and they double as the stable opaque IDs a Subsonic
adapter would want.

### Two library roots

```
/music              read-only bind mount   ← your existing collection
/library/downloads  read-write             ← YouTube promotions land here
```

Mounting the collection `:ro` enforces the no-mutation decision at the kernel
level rather than trusting application code. Promotions need a writable
destination, so they get their own root.

---

## Data model

### Identity

```sql
CREATE TYPE user_role AS ENUM ('admin', 'user');

users (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  email         citext NOT NULL UNIQUE,
  display_name  text NOT NULL DEFAULT '',
  avatar_url    text,
  google_sub    text UNIQUE,   -- stable subject from Google
  oidc_sub      text UNIQUE,   -- stable subject from the generic provider
  password_hash text,          -- local accounts only; NULL for SSO
  role          user_role NOT NULL DEFAULT 'user',
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  disabled_at   timestamptz,

  CONSTRAINT users_has_credential CHECK (
    google_sub IS NOT NULL OR oidc_sub IS NOT NULL OR password_hash IS NOT NULL
  )
)

sessions (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id      uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  token_hash   bytea NOT NULL UNIQUE,
  csrf_token   text NOT NULL,
  user_agent   text,
  ip           inet,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL
)
```

**Email is the linking key; the provider subject is the identity.** Sign-in
looks up `google_sub`/`oidc_sub` first, because a subject is stable even when
the user changes their address at the provider — matching on email first would
strand a renamed user with a duplicate account. Email is the fallback, and it is
what lets one person sign in via Google and later via a self-hosted IdP and land
on the same account.

`citext` makes both email lookups and uniqueness case-insensitive, which matters
because identity providers disagree about case normalisation. The CHECK
constraint refuses an account with no way to sign in. All timestamps are
`timestamptz` — never naive.

`role` is `admin` (manage users and roots, trigger scans) or `user`. The first
person to sign in claims the instance and becomes administrator; everyone after
must be on `ECHO_ALLOWED_EMAILS`. See [Authentication](#authentication).

### Library

```sql
library_roots (id uuid PK, path text UNIQUE, writable bool, enabled bool)

artists (
  id        uuid PRIMARY KEY DEFAULT uuidv7(),
  name      text NOT NULL,
  norm_name text NOT NULL,          -- casefolded, depunctuated, article-stripped
  sort_name text,
  mbid      uuid,
  UNIQUE (norm_name)
)

albums (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  name            text NOT NULL,
  norm_name       text NOT NULL,
  album_artist_id uuid REFERENCES artists,
  year            int,
  cover_art_id    uuid REFERENCES cover_art,
  disc_count      int NOT NULL DEFAULT 1,
  UNIQUE (norm_name, album_artist_id)
)

genres (id uuid PK, name citext UNIQUE)

tracks (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  root_id         uuid NOT NULL REFERENCES library_roots,
  rel_path        text NOT NULL,
  size            bigint NOT NULL,
  mtime           timestamptz NOT NULL,
  content_hash    bytea NOT NULL,
  duration_ms     int, bitrate int, sample_rate int, channels smallint,
  codec           text, suffix text,
  title           text, track_no int, disc_no int, year int,
  album_id        uuid REFERENCES albums,
  artist_id       uuid REFERENCES artists,
  album_artist_id uuid REFERENCES artists,
  cover_art_id    uuid REFERENCES cover_art,
  missing_at      timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (root_id, rel_path)
)

track_genres    (track_id uuid, genre_id uuid, PRIMARY KEY (track_id, genre_id))
track_overrides (track_id uuid PK REFERENCES tracks ON DELETE CASCADE,
                 title, artist_name, album_name, album_artist_name,
                 genre, year, track_no, disc_no, updated_at)
cover_art       (id uuid PK, source text, hash bytea UNIQUE, path text, width int, height int)
```

Indexes worth naming up front:

```sql
CREATE INDEX ON tracks (album_id, disc_no, track_no) WHERE missing_at IS NULL;
CREATE INDEX ON tracks (artist_id)                   WHERE missing_at IS NULL;
CREATE INDEX ON tracks (content_hash);               -- move detection
```

The partial indexes matter: every browse query filters out missing tracks, and
excluding them keeps the index tight even after a drive goes offline and marks
thousands of rows.

`artists`, `albums`, and `genres` are **derived** during scan, not
user-authored. Reconciliation is by normalized name (casefold, strip
punctuation and leading articles).

Overrides are applied at read time through a view, so every query and the
search index see one consistent set of values:

```sql
CREATE VIEW tracks_effective AS
SELECT t.id, t.root_id, t.rel_path, t.duration_ms, /* … */
       COALESCE(o.title,  t.title)    AS title,
       COALESCE(o.year,   t.year)     AS year,
       COALESCE(o.track_no, t.track_no) AS track_no
       /* … */
FROM tracks t LEFT JOIN track_overrides o ON o.track_id = t.id;
```

**Subsonic compatibility:** IDs are stable opaque strings, and tracks carry
`duration`/`bitrate`/`suffix`/`size`/`track`/`discNumber` with albums carrying
`coverArt`. That's the full set a Subsonic adapter would need, and all of it is
worth having regardless.

### Per-user state

```sql
CREATE TYPE entity_type AS ENUM ('track', 'album', 'artist', 'playlist');

playlists       (id uuid PK, user_id uuid, name, description, public bool,
                 created_at, updated_at)
playlist_tracks (playlist_id uuid, track_id uuid, position int, added_at,
                 PRIMARY KEY (playlist_id, position))
favorites       (user_id uuid, entity_type entity_type, entity_id uuid, created_at,
                 PRIMARY KEY (user_id, entity_type, entity_id))
plays           (id uuid PK, user_id uuid, track_id uuid,
                 played_at timestamptz, ms_played int, source text)
offline_marks   (user_id uuid, entity_type entity_type, entity_id uuid, created_at,
                 PRIMARY KEY (user_id, entity_type, entity_id))
```

`offline_marks` is the server-side record of *intent* to have something
available offline, so the choice syncs across a user's devices. The bytes
themselves live only in each browser's Cache API.

### YouTube

```sql
CREATE TYPE yt_state AS ENUM ('pending','downloading','ready','failed','evicted');

yt_items (
  id uuid PK, video_id text UNIQUE NOT NULL,
  title text, uploader text, uploader_id text,
  duration_ms int, thumbnail_url text,
  state yt_state NOT NULL DEFAULT 'pending',
  cached_path text, bytes bigint,
  cached_at timestamptz, last_accessed_at timestamptz, expires_at timestamptz,
  promoted_track_id uuid REFERENCES tracks, error text,
  created_at timestamptz NOT NULL DEFAULT now()
)

CREATE INDEX ON yt_items (expires_at) WHERE state = 'ready';
CREATE INDEX ON yt_items (last_accessed_at) WHERE state = 'ready';
```

### Jobs

```sql
CREATE TYPE job_state AS ENUM ('queued','running','done','failed');

jobs (
  id uuid PRIMARY KEY DEFAULT uuidv7(),
  type text NOT NULL, payload jsonb NOT NULL,
  priority int NOT NULL DEFAULT 0,
  state job_state NOT NULL DEFAULT 'queued',
  attempts int NOT NULL DEFAULT 0, error text,
  dedupe_key text UNIQUE,
  run_after timestamptz NOT NULL DEFAULT now(),
  created_at timestamptz NOT NULL DEFAULT now(),
  started_at timestamptz, finished_at timestamptz
)

CREATE INDEX ON jobs (priority DESC, run_after) WHERE state = 'queued';
```

Types: `scan_root`, `scan_file`, `yt_download`, `yt_promote`, `cache_evict`,
`transcode`.

Postgres-backed, no Redis. Workers claim with the standard pattern:

```sql
UPDATE jobs SET state = 'running', started_at = now(), attempts = attempts + 1
WHERE id = (
  SELECT id FROM jobs
  WHERE state = 'queued' AND run_after <= now()
  ORDER BY priority DESC, run_after
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING *;
```

`SKIP LOCKED` lets many workers drain the queue concurrently without contention
or double-claiming. `LISTEN`/`NOTIFY` on insert wakes idle workers immediately,
so latency doesn't depend on a poll interval. `dedupe_key` collapses duplicate
enqueues — the filesystem watcher firing three events for one file write should
produce one `scan_file` job, not three.

### Search

A denormalized `track_search` table, maintained by the application whenever a
track is scanned or an override is edited:

```sql
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS unaccent;

track_search (
  track_id uuid PRIMARY KEY REFERENCES tracks ON DELETE CASCADE,
  haystack text NOT NULL,             -- "title artist album album_artist genre"
  tsv tsvector GENERATED ALWAYS AS (
    to_tsvector('simple', unaccent(haystack))
  ) STORED
);

CREATE INDEX ON track_search USING gin (tsv);
CREATE INDEX ON track_search USING gin (haystack gin_trgm_ops);
```

**Two indexes because music search has two modes.** `tsvector` gives ranked
whole-word matching and handles multi-term queries properly. Trigram gives
substring and fuzzy matching — which is what actually saves you when someone
types `radiohed`, `bjork` for `Björk`, or `beatles` expecting `The Beatles`.
Query strategy: try `tsv @@ websearch_to_tsquery(...)` first, and widen to
trigram when it returns fewer than three hits.

**The fallback uses `word_similarity` (`<%`), not `similarity` (`%`).**
`similarity` compares whole strings, so a short query against a long haystack
("Airbag Radiohead OK Computer Alternative Rock") scores far below any usable
threshold and matches nothing at all. `word_similarity` asks whether the query
closely matches some run of words *inside* the haystack, which is what a search
box actually means. Both are backed by the same GIN trigram index.

The two are separate queries rather than one OR-ed predicate: combining them
stops Postgres using either index and turns every search into a sequential
scan.

`'simple'` rather than `'english'` deliberately — English stemming mangles
band names and non-English titles, and there's nothing to gain from stemming
proper nouns.

Maintained explicitly at write time rather than by trigger: the effective
values span `tracks`, `track_overrides`, `artists`, `albums`, and
`track_genres`, and trigger fan-out across five tables becomes unmaintainable
fast. The scanner already knows when it has written a track, so it writes the
search row in the same transaction.

---

## Scanner

**Full scan.** Walk each root; for each audio file compare `(size, mtime)`
against the DB and skip unchanged files. This makes rescans of a 50k-track
library take seconds.

**Changed or new files** go to a worker pool (`GOMAXPROCS` goroutines) that
reads tags with `dhowden/tag` and extracts embedded art. Results are written in
batched transactions of ~500 rows; the initial import of a large library uses
`COPY` via pgx's `CopyFrom`, which is an order of magnitude faster than
batched inserts for the first 50k rows.

Scan writes and playback reads never contend, so a full rescan can run at any
time without degrading playback.

**Move detection.** `content_hash` is xxhash64 over `size || first 64KiB ||
last 64KiB` — cheap, and collision risk is irrelevant for this purpose. Same
hash at a new path means update `rel_path` and keep the track ID, which
preserves playlists and history across reorganization.

A matching hash is necessary but not sufficient: a **copy** has identical
content while the original is still present, and treating that as a move would
relocate the original's row and leave the real file with none. The source file
must actually be gone before a match counts as a move.

**Scheduling arithmetic belongs to the database.** `run_after`, retry backoff,
and every purge cutoff are computed with `now() + interval` server-side. A
client-supplied timestamp makes scheduling depend on the app and database clocks
agreeing — a host a few milliseconds ahead produces jobs that are not claimable
when their NOTIFY arrives, so each one silently waits for the next poll instead.

**Deletions** set `missing_at` rather than removing the row, so a temporarily
unmounted drive doesn't destroy playlists. Rows are purged after 30 days or on
explicit admin action.

**Watching.** `fsnotify` with manual recursive registration, a 2s debounce, and
per-path coalescing. New directories are registered as they appear. Falls back
to periodic full scans when the watcher can't attach — NFS mounts, which is a
realistic homelab case.

**Cover art** resolves embedded art first, then sibling `cover|folder|front.{jpg,png}`.
Extracted art is written to the cache dir keyed by content hash, so the same
art shared across an album is stored once.

---

## Streaming

**Originals** are served with `http.ServeContent` over an `*os.File`, which
handles `Range`, `If-Range`, `If-Modified-Since`, ETags, and multipart ranges
correctly. Browsers play MP3, M4A/AAC, Opus, OGG, and FLAC natively, so this is
the path for essentially the whole library.

**ServeContent does not invent an ETag.** It honours `If-None-Match` and
`If-Range` when one is present, but supplies none of its own, so it has to be
set explicitly or every re-listen refetches the whole track. The scanner's
content hash is the right value: stable across restarts, different the moment
the file changes.

**Transcoding** is for unsupported codecs (WMA, WavPack, some ALAC) only. A
bandwidth-saver profile was considered and dropped: it needs transcode profiles,
a per-user preference, and cache keys per (track, format, bitrate), for a
benefit that only appears on mobile data. Transcoded output goes to a **cache file first**,
then gets served with `ServeContent` like anything else:

```
GET /api/tracks/:id/stream?format=opus&bitrate=96
  → cache hit?  serve
  → miss?       ffmpeg → cache/transcode/<track>-<fmt>-<rate>.opus → serve
```

Piping ffmpeg straight to the response would kill seeking — the length is
unknown and the stream isn't rewindable. Paying a few hundred milliseconds on
first play to get a seekable file is the right trade. LRU eviction against a
configurable size cap.

### Authentication

Sign-in is delegated to an identity provider: **Google**, a **generic OIDC
provider** (Authentik, Keycloak, Pocket ID, …), or both. Each runs the
authorization-code flow with **PKCE**, via `coreos/go-oidc` over
`golang.org/x/oauth2`. Discovery happens at startup, so a wrong issuer or an
unreachable provider fails the deployment rather than somebody's first sign-in.

One `ssoProvider` type serves both. They differ in exactly three ways: the
discovery issuer, which subject column links the account, and how strictly
`email_verified` is enforced. Google always sets the claim, so it is required.
Self-hosted providers frequently omit it, so the generic provider tolerates
absence — but an explicit `email_verified: false` is refused either way, because
a provider stating an address is unverified must never be treated as proof of
that address.

**Issuer normalisation.** An OIDC issuer is an exact-match identifier and
providers disagree about trailing slashes — Authentik's ends with one,
Keycloak's does not. When that slash is the only difference, discovery has
already proven which form is canonical, so startup retries with it and logs a
warning rather than refusing to boot over one character.

**Account resolution**, in order: match the provider subject; else link to an
existing account with the same email; else create one. The first account created
becomes administrator, and every later sign-up must be on
`ECHO_ALLOWED_EMAILS`. The allowlist gates *sign-up*, never sign-in — existing
users always keep working.

**Local password accounts** are off by default (`ECHO_LOCAL_AUTH`). They exist
for two cases: bringing an instance up before an identity provider is
configured, and a break-glass administrator for when the provider is
unreachable. Passwords are argon2id at OWASP's 19 MiB / t=2 configuration —
deliberately not the heavier 64 MiB setting, since sign-in is unauthenticated
and each attempt costs the server its memory cost up front.

**Failures redirect rather than render.** The callback sends the browser to
`/signin?error=…`; a bare 403 body would strand a user mid-redirect with no way
back.

### Auth on media URLs

`<audio src>` cannot send an `Authorization` header, and neither can a service
worker replaying a cached range request cleanly.

**Sessions are therefore HttpOnly cookies**, `Secure`, `SameSite=Lax`. Media
URLs work in plain `<audio>` tags and in the service worker with no special
handling. Mutating endpoints require a double-submit CSRF token, since `Lax`
alone does not cover same-site sub-requests.

The alternative — short-lived signed URL tokens — was rejected: it complicates
offline caching, since a cached response outlives its token.

---

## YouTube

### Search

```
yt-dlp --flat-playlist --dump-json "ytsearch20:<query>"
```

Metadata only, no media fetch. Results are ~300ms and get cached briefly by
query string. Library results and YouTube results are visually distinct in the
UI — YouTube rows show a cache-state badge.

### Playback

v1 **downloads to cache, then serves**, with a progress indicator:

```
yt-dlp -f bestaudio --extract-audio --audio-format opus
       --embed-metadata --embed-thumbnail
       --sponsorblock-remove sponsor,intro,outro,music_offtopic
       -o cache/youtube/<video_id>.%(ext)s
```

A 4-minute Opus track is ~4 MB and lands in a second or two. Once cached it is
a normal file on disk, so seeking, range requests, and service-worker caching
all work with zero extra machinery.

Proxy-streaming the direct CDN URL (`yt-dlp -g`) would start faster, but the
URLs are IP-bound and expire in hours, seeking is fragile, and it must be
proxied rather than redirected because the client IP differs from the server's.
It's an optimization to revisit if the download wait actually annoys.

**On "no ads":** `bestaudio` is the raw audio stream — there is no ad insertion
in it, so ads simply don't exist on this path. The `--sponsorblock-remove`
flags handle the different problem of host-read sponsor segments baked into the
audio itself.

### Cache lifecycle

TTL is **sliding on `last_accessed_at`, 48h default**, with a hard cap of 14
days and a total size cap (default 5 GB, LRU). A sliding window matches intent
better than a fixed one — something you play daily shouldn't vanish mid-week
because you first heard it 49 hours ago.

A janitor job runs every 15 minutes: expire, then evict by LRU until under the
size cap. Promoted items are exempt.

### Promote to library

Copies the cached file to `/library/downloads/<uploader>/<title>.opus`, embeds
final metadata, inserts a normal `tracks` row, and sets
`yt_items.promoted_track_id`. From that point it is an ordinary library track
and survives cache eviction. Playlists referencing the cached item are
repointed to the new track ID.

### Operational risks

- **Bot detection.** YouTube increasingly requires cookies or PO tokens.
  Residential homelab IPs are usually fine; if not, support a mounted
  `cookies.txt` via `--cookies`. Datacenter IPs are frequently blocked outright.
- **yt-dlp churn.** Extraction breaks every few weeks. Pin the version in the
  image, expose it on an admin endpoint, and make rebuilding the image the
  update path. Do not couple yt-dlp's release cadence to Echo's.
- **Terms of service.** Downloading from YouTube is contrary to its ToS. This
  is a personal self-hosted tool; noting it so the choice is explicit.

---

## API

`/api/v1`, JSON, cursor-paginated. huma generates the OpenAPI spec from Go
types; `make types` regenerates the client's TypeScript.

```
GET    /auth/providers              → which sign-in buttons to show
POST   /auth/login                  → local accounts only; sets session cookie
POST   /auth/logout
POST   /auth/password               → local accounts only
GET    /auth/me

GET    /tracks       ?q=&genre=&artist=&album=&year=&sort=&cursor=&limit=
GET    /tracks/:id
PATCH  /tracks/:id                  → writes track_overrides
GET    /tracks/:id/stream           ?format=&bitrate=
GET    /albums       ?q=&artist=&genre=&year=&cursor=
GET    /albums/:id                  → album + tracks
GET    /artists      ?q=&cursor=
GET    /artists/:id
GET    /genres                      → with counts
GET    /art/:id                     ?size=

GET    /search       ?q=            → tracks, albums, artists in one response

CRUD   /playlists, /playlists/:id/tracks
PUT    /favorites/:type/:id
DELETE /favorites/:type/:id
POST   /plays                       → scrobble
GET    /history

GET    /offline/marks
PUT    /offline/marks/:type/:id
DELETE /offline/marks/:type/:id

GET    /youtube/search   ?q=
POST   /youtube/prepare  {video_id}  → enqueue download, returns job
GET    /youtube/:video_id            → state, progress, expires_at
GET    /youtube/:video_id/stream
POST   /youtube/:video_id/promote

ADMIN  /admin/users, /admin/roots, /admin/scan, /admin/jobs, /admin/version
```

Browser-facing OAuth endpoints live at the **site root**, not under the API
prefix, because they are browser navigations rather than API calls — the
identity provider redirects the user's browser to them and the response is a
302:

```
GET /auth/google           GET /auth/google/callback
GET /auth/oidc             GET /auth/oidc/callback
```

Pagination is keyset on `(sort_key, id)`, not offset — offset pagination
degrades badly past a few thousand rows and double-serves items when the
library changes mid-scroll.

---

## Web client

```
React 19 + Vite + TypeScript
TanStack Query      server state, cache, optimistic updates
TanStack Virtual    50k-row lists
Zustand             player state (queue, position, shuffle, repeat)
Workbox             service worker
```

**One `<audio>` element for the entire app lifetime**, owned by the Zustand
store. iOS only reliably permits playback on an element that was first started
by a user gesture, so creating an element per track breaks autoplay-next.
Preloading the next track uses a second, hidden element that swaps roles at
boundaries.

**Media Session API** for lock-screen and hardware controls — artwork, title,
seek, next/previous.

### Offline

Marked content is downloaded into the **Cache API**, not IndexedDB — the
service worker intercepts audio requests naturally and can serve cached
responses to the same URLs.

**The gotcha:** `<audio>` issues `Range` requests, but `cache.match()` returns
the full `200` response. The service worker must parse the `Range` header,
slice the body itself, and synthesize a `206` with correct `Content-Range` and
`Accept-Ranges`. Getting this wrong looks like "offline playback works but
seeking is broken", which is an unpleasant thing to debug late.

Quota is requested via `navigator.storage.persist()` and surfaced in the UI
from `navigator.storage.estimate()`.

**iOS caveat:** background audio in an installed PWA is unreliable — Safari
suspends aggressively when the app is backgrounded. This is the single most
likely reason to fall back to the Subsonic adapter, and the reason the schema
stays compatible.

---

## Deployment

Multi-stage build: Node builds the web client → Go embeds it via `embed.FS` and
compiles → runtime image carries `ffmpeg`, `yt-dlp`, and `ca-certificates`.
Single binary, `CGO_ENABLED=0`.

```yaml
services:
  echo:
    image: echo:latest
    ports: ["8080:8080"]
    depends_on:
      db: { condition: service_healthy }
    volumes:
      - /srv/music:/music:ro
      - echo-cache:/cache              # transcodes, yt cache, extracted art
      - echo-library:/library/downloads
    environment:
      ECHO_DATABASE_URL: postgres://echo:echo@db:5432/echo?sslmode=disable
      ECHO_LIBRARY_ROOTS: /music,/library/downloads
      ECHO_CACHE_DIR: /cache
      ECHO_YT_CACHE_TTL: 48h
      ECHO_YT_CACHE_MAX_BYTES: 5GB
      ECHO_TRANSCODE_CACHE_MAX_BYTES: 10GB

  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: echo
      POSTGRES_PASSWORD: echo
      POSTGRES_DB: echo
    volumes: [echo-db:/var/lib/postgresql/data]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U echo"]
      interval: 5s
      retries: 10
```

Migrations run on startup via goose, guarded by a Postgres advisory lock so
concurrent replicas can't race.

### Homelab k8s notes

Not shipped as charts, but the constraints to carry over by hand:

- **Postgres**: CloudNativePG if you want backups and failover handled, or a
  plain StatefulSet. Do not put it on the same PVC as the caches.
- **PVCs** for `/cache` and `/library/downloads`; mount the existing music
  share read-only.
- `startupProbe` with a generous window — the first scan of a large library
  takes minutes. Better: don't scan on the request path at all; the scan is a
  job, so readiness only needs the DB and HTTP listener.
- Advisory-lock the migration step so a rollout doesn't run goose twice.
- Backups are now `pg_dump` or WAL archiving rather than copying a file. The
  caches are disposable and should be excluded; `/library/downloads` is not
  and should be backed up with your music.

### Scaling, honestly

Postgres removes the *database* ceiling, and the API pods become genuinely
replicable. But the real constraint moves to the filesystem, so it's worth
being precise about what scales and what doesn't:

| Component | Replicable | Why |
|---|---|---|
| API / streaming | Yes | Stateless given shared storage; `ServeContent` over an RWX mount |
| Job workers | Yes | `SKIP LOCKED` makes N-way consumption safe |
| Filesystem watcher | **No** | N watchers on one share = N duplicate events |
| Transcode / YT cache | Conditional | Per-pod local disk means N× storage and cold caches |

Two things to fix before running more than one replica:

1. **The watcher must be a singleton** — either a separate single-replica
   Deployment, or leader election via a Postgres advisory lock. The
   `dedupe_key` on `jobs` limits the damage if this is botched, but doesn't
   make it correct.
2. **Caches must be shared or partitioned** — an RWX volume, or S3/MinIO with
   the cache layer behind an interface so the storage backend is swappable.
   Defining that interface in M0 is cheap; retrofitting it is not.

Neither is worth building now. Both are worth not designing yourself out of,
which mostly means: keep cache access behind a `BlobStore` interface, and keep
the watcher in its own component rather than wired into the API server's
lifecycle.

---

## Milestones

| | Scope | Exit criteria |
|---|---|---|
| **M0** ✅ | Skeleton: config, goose migrations, sqlc, huma+OpenAPI, `make types`, `BlobStore` iface, Dockerfile, compose, CI | `docker compose up` brings up Postgres + app and serves a health endpoint; testcontainers green in CI |
| **M1** ✅ | Auth + users. Google + OIDC sign-in (PKCE), session cookies, CSRF, allowlist, user CRUD | Sign in through a real IdP in a browser; roles enforced |
| **M2** ✅ | Scanner. Walk, tag read, art extraction, move detection, fsnotify, `SKIP LOCKED` job queue | 50k-track library scans; moves preserve IDs; playback unaffected during a scan |
| **M3** ✅ | Library API + search. tsvector + trigram, facets, keyset pagination, overrides | Filter by genre/artist/album; `radiohed` finds Radiohead; edits persist to overrides |
| **M4** ✅ | Streaming + player. ServeContent, transcode cache, React shell, virtualized lists, Media Session | Full playback with seeking on desktop and mobile browsers |
| **M5** ✅ | Playlists, favorites, history | Per-user state working across two accounts |
| **M6** ✅ | YouTube. Search, download-to-cache, TTL janitor, promote | Play a YouTube result; it expires on schedule; promotion survives eviction |
| **M7** | PWA offline. Workbox, Cache API audio, Range-slicing SW, quota UI | Airplane mode plays marked albums, seeking included |
| **M8** | Polish. Admin UI, transcode profiles, scrobble targets, keyboard shortcuts | — |

Auth lands at M1 rather than late because every per-user table depends on
`users` existing, and retrofitting `user_id` across playlists and history is
the exact migration this ordering avoids.

## Open questions

1. **Transcode policy** — transcode only on codec incompatibility, or also
   offer a "data saver" profile for mobile? Affects whether transcode profiles
   are per-user settings.
2. ~~**Play-count semantics**~~ — settled: Last.fm's rule, half the track or
   four minutes, whichever comes first, validated server-side against the
   duration Echo knows rather than one the client reports.
3. **Compilation handling** — `albumartist` is unreliable in the wild. Group by
   folder as a fallback signal, or trust tags and accept fragmentation?
4. **Scrobbling** — deferred. History is local for now; the `plays` table
   already carries what an external service would need, so adding it later
   needs no schema change.
