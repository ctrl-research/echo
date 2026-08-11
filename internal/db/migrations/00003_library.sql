-- +goose Up

-- ---- roots -----------------------------------------------------------------

CREATE TABLE library_roots (
    id                    uuid        PRIMARY KEY DEFAULT uuidv7(),
    path                  text        NOT NULL UNIQUE,
    -- The first writable root receives promoted YouTube downloads. Collection
    -- roots are mounted read-only and are never written to.
    writable              bool        NOT NULL DEFAULT false,
    enabled               bool        NOT NULL DEFAULT true,
    last_scan_started_at  timestamptz,
    last_scan_finished_at timestamptz,
    last_scan_error       text,
    created_at            timestamptz NOT NULL DEFAULT now()
);

-- ---- cover art -------------------------------------------------------------

CREATE TABLE cover_art (
    id         uuid        PRIMARY KEY DEFAULT uuidv7(),
    -- Content hash of the image bytes. Album art is duplicated across every
    -- track on an album, so addressing it by content stores it once.
    hash       bytea       NOT NULL UNIQUE,
    -- Key into the blob store, not a filesystem path: derived data may move to
    -- object storage without a schema change.
    blob_key   text        NOT NULL,
    source     text        NOT NULL,   -- 'embedded' | 'sidecar'
    mime       text        NOT NULL,
    width      int,
    height     int,
    bytes      bigint      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- ---- artists, albums, genres ------------------------------------------------

CREATE TABLE artists (
    id         uuid        PRIMARY KEY DEFAULT uuidv7(),
    name       text        NOT NULL,
    -- Casefolded, unaccented, depunctuated, article-stripped. Reconciliation
    -- happens on this, so "The Beatles" and "Beatles" are one artist.
    norm_name  text        NOT NULL UNIQUE,
    sort_name  text,
    mbid       uuid,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE albums (
    id              uuid        PRIMARY KEY DEFAULT uuidv7(),
    name            text        NOT NULL,
    norm_name       text        NOT NULL,
    album_artist_id uuid        REFERENCES artists (id),
    year            int,
    cover_art_id    uuid        REFERENCES cover_art (id),
    disc_count      int         NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Two different artists may both have an album called "Greatest Hits".
    -- NULLS NOT DISTINCT so compilations with no album artist still collapse
    -- to one row rather than one per track.
    UNIQUE NULLS NOT DISTINCT (norm_name, album_artist_id)
);

CREATE TABLE genres (
    id   uuid   PRIMARY KEY DEFAULT uuidv7(),
    name citext NOT NULL UNIQUE
);

-- ---- tracks -----------------------------------------------------------------

CREATE TABLE tracks (
    id              uuid        PRIMARY KEY DEFAULT uuidv7(),
    root_id         uuid        NOT NULL REFERENCES library_roots (id) ON DELETE CASCADE,
    rel_path        text        NOT NULL,
    size            bigint      NOT NULL,
    mtime           timestamptz NOT NULL,
    -- xxhash64 over size, the first 64 KiB, and the last 64 KiB. Cheap enough
    -- to compute for every file, and its only job is move detection — a
    -- collision would misattribute a move, not corrupt anything.
    content_hash    bytea       NOT NULL,

    duration_ms     int,
    bitrate         int,
    sample_rate     int,
    channels        smallint,
    codec           text,
    suffix          text        NOT NULL,

    title           text        NOT NULL DEFAULT '',
    track_no        int,
    disc_no         int,
    year            int,

    album_id        uuid        REFERENCES albums (id),
    artist_id       uuid        REFERENCES artists (id),
    album_artist_id uuid        REFERENCES artists (id),
    cover_art_id    uuid        REFERENCES cover_art (id),

    -- Set rather than deleted when a file disappears, so a temporarily
    -- unmounted drive does not destroy playlists and history.
    missing_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    UNIQUE (root_id, rel_path)
);

-- Browse queries always exclude missing tracks; a partial index stays small
-- even after a drive goes offline and marks thousands of rows.
CREATE INDEX tracks_album_idx  ON tracks (album_id, disc_no, track_no) WHERE missing_at IS NULL;
CREATE INDEX tracks_artist_idx ON tracks (artist_id) WHERE missing_at IS NULL;
CREATE INDEX tracks_hash_idx   ON tracks (content_hash);
CREATE INDEX tracks_root_idx   ON tracks (root_id);

CREATE TABLE track_genres (
    track_id uuid NOT NULL REFERENCES tracks (id) ON DELETE CASCADE,
    genre_id uuid NOT NULL REFERENCES genres (id) ON DELETE CASCADE,
    PRIMARY KEY (track_id, genre_id)
);

CREATE INDEX track_genres_genre_idx ON track_genres (genre_id);

-- Metadata edits live here, never in the audio files. Applied at read time
-- through tracks_effective, so a rescan never fights a user's correction.
CREATE TABLE track_overrides (
    track_id          uuid PRIMARY KEY REFERENCES tracks (id) ON DELETE CASCADE,
    title             text,
    artist_name       text,
    album_name        text,
    album_artist_name text,
    genre             text,
    year              int,
    track_no          int,
    disc_no           int,
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE VIEW tracks_effective AS
SELECT t.id,
       t.root_id,
       t.rel_path,
       t.size,
       t.mtime,
       t.content_hash,
       t.duration_ms,
       t.bitrate,
       t.sample_rate,
       t.channels,
       t.codec,
       t.suffix,
       t.album_id,
       t.artist_id,
       t.album_artist_id,
       t.cover_art_id,
       t.missing_at,
       t.created_at,
       t.updated_at,
       COALESCE(o.title, t.title)                    AS title,
       COALESCE(o.track_no, t.track_no)              AS track_no,
       COALESCE(o.disc_no, t.disc_no)                AS disc_no,
       COALESCE(o.year, t.year)                      AS year,
       COALESCE(o.artist_name, ar.name, '')          AS artist_name,
       COALESCE(o.album_name, al.name, '')           AS album_name,
       COALESCE(o.album_artist_name, aa.name, '')    AS album_artist_name
FROM tracks t
LEFT JOIN track_overrides o  ON o.track_id = t.id
LEFT JOIN artists ar         ON ar.id = t.artist_id
LEFT JOIN albums al          ON al.id = t.album_id
LEFT JOIN artists aa         ON aa.id = t.album_artist_id;

-- ---- search -----------------------------------------------------------------

-- Denormalised, maintained by the writer rather than by trigger: the effective
-- values span five tables, and trigger fan-out across all of them becomes
-- unmaintainable. The scanner already knows when it has written a track.
CREATE TABLE track_search (
    track_id uuid PRIMARY KEY REFERENCES tracks (id) ON DELETE CASCADE,
    haystack text NOT NULL,
    tsv      tsvector GENERATED ALWAYS AS (
        to_tsvector('simple', immutable_unaccent(haystack))
    ) STORED
);

-- Two indexes because music search has two modes. tsvector gives ranked
-- whole-word matching; trigram gives substring and fuzzy matching, which is
-- what saves you when somebody types "radiohed" or "bjork".
CREATE INDEX track_search_tsv_idx  ON track_search USING gin (tsv);
CREATE INDEX track_search_trgm_idx ON track_search USING gin (haystack gin_trgm_ops);

-- ---- jobs -------------------------------------------------------------------

CREATE TYPE job_state AS ENUM ('queued', 'running', 'done', 'failed');

CREATE TABLE jobs (
    id          uuid        PRIMARY KEY DEFAULT uuidv7(),
    type        text        NOT NULL,
    payload     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    priority    int         NOT NULL DEFAULT 0,
    state       job_state   NOT NULL DEFAULT 'queued',
    attempts    int         NOT NULL DEFAULT 0,
    max_attempts int        NOT NULL DEFAULT 3,
    error       text,
    -- Collapses duplicate enqueues: a filesystem watcher firing three events
    -- for one file write must produce one job, not three.
    dedupe_key  text        UNIQUE,
    run_after   timestamptz NOT NULL DEFAULT now(),
    created_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    finished_at timestamptz
);

CREATE INDEX jobs_claim_idx ON jobs (priority DESC, run_after) WHERE state = 'queued';
CREATE INDEX jobs_state_idx ON jobs (state, created_at DESC);

-- +goose Down
DROP TABLE jobs;
DROP TYPE job_state;
DROP TABLE track_search;
DROP VIEW tracks_effective;
DROP TABLE track_overrides;
DROP TABLE track_genres;
DROP TABLE tracks;
DROP TABLE genres;
DROP TABLE albums;
DROP TABLE artists;
DROP TABLE cover_art;
DROP TABLE library_roots;
