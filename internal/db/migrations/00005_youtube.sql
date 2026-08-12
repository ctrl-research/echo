-- +goose Up

CREATE TYPE yt_state AS ENUM ('pending', 'downloading', 'ready', 'failed', 'evicted');

CREATE TABLE yt_items (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    video_id     text        NOT NULL UNIQUE,
    title        text        NOT NULL DEFAULT '',
    uploader     text        NOT NULL DEFAULT '',
    duration_ms  int,
    thumbnail_url text,

    state        yt_state    NOT NULL DEFAULT 'pending',
    error        text,

    blob_key     text,
    bytes        bigint,
    cached_at    timestamptz,
    -- The TTL slides on this rather than on cached_at. Something played daily
    -- should not vanish mid-week because it was first heard 49 hours ago.
    last_accessed_at timestamptz,
    -- A hard ceiling so a single much-played item cannot pin cache space
    -- indefinitely.
    expires_at   timestamptz,

    -- Set once the item has been copied into the library. A promoted item is
    -- exempt from eviction: its bytes now live under a library root, not in
    -- the disposable cache.
    promoted_track_id uuid REFERENCES tracks (id) ON DELETE SET NULL,

    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- The eviction sweep scans by expiry and by least-recently-used, and only ever
-- considers items that actually hold bytes.
CREATE INDEX yt_items_expiry_idx ON yt_items (expires_at)
    WHERE state = 'ready' AND promoted_track_id IS NULL;
CREATE INDEX yt_items_lru_idx ON yt_items (last_accessed_at)
    WHERE state = 'ready' AND promoted_track_id IS NULL;
CREATE INDEX yt_items_state_idx ON yt_items (state);

-- +goose Down
DROP TABLE yt_items;
DROP TYPE yt_state;
