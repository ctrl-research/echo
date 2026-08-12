-- +goose Up

CREATE TABLE playlists (
    id          uuid        PRIMARY KEY DEFAULT uuidv7(),
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name        text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    -- Public playlists are readable by any signed-in user; the owner is still
    -- the only one who can change them.
    public      bool        NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT playlists_name_length CHECK (length(name) BETWEEN 1 AND 200)
);

CREATE INDEX playlists_user_idx ON playlists (user_id, lower(name));

-- A surrogate key, not (playlist_id, track_id): the same song may legitimately
-- appear twice in one playlist, and a natural key would forbid that.
CREATE TABLE playlist_tracks (
    id          uuid        PRIMARY KEY DEFAULT uuidv7(),
    playlist_id uuid        NOT NULL REFERENCES playlists (id) ON DELETE CASCADE,
    track_id    uuid        NOT NULL REFERENCES tracks (id) ON DELETE CASCADE,
    position    int         NOT NULL,
    added_at    timestamptz NOT NULL DEFAULT now(),

    -- Deferrable so a reorder can rewrite every position inside one
    -- transaction. An immediate constraint would reject the intermediate
    -- states that any bulk renumbering passes through.
    CONSTRAINT playlist_tracks_position_unique
        UNIQUE (playlist_id, position) DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX playlist_tracks_playlist_idx ON playlist_tracks (playlist_id, position);
CREATE INDEX playlist_tracks_track_idx ON playlist_tracks (track_id);

CREATE TYPE favorite_entity AS ENUM ('track', 'album', 'artist');

CREATE TABLE favorites (
    user_id     uuid            NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    entity_type favorite_entity NOT NULL,
    entity_id   uuid            NOT NULL,
    created_at  timestamptz     NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, entity_type, entity_id)
);

-- Answers "which of these tracks are favourited", which every listing needs.
CREATE INDEX favorites_lookup_idx ON favorites (user_id, entity_type, entity_id);

CREATE TABLE plays (
    id         uuid        PRIMARY KEY DEFAULT uuidv7(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Plays outlive the file. A track deleted from disk should not erase the
    -- fact that it was listened to, so this is nullable rather than cascading.
    track_id   uuid        REFERENCES tracks (id) ON DELETE SET NULL,
    played_at  timestamptz NOT NULL DEFAULT now(),
    ms_played  int         NOT NULL,
    source     text        NOT NULL DEFAULT 'library'
);

CREATE INDEX plays_user_time_idx ON plays (user_id, played_at DESC);
CREATE INDEX plays_track_idx ON plays (track_id) WHERE track_id IS NOT NULL;

-- +goose Down
DROP TABLE plays;
DROP TABLE favorites;
DROP TYPE favorite_entity;
DROP TABLE playlist_tracks;
DROP TABLE playlists;
