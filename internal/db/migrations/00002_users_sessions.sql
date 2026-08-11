-- +goose Up

CREATE TYPE user_role AS ENUM ('admin', 'user');

CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT uuidv7(),
    -- Email is the identity across providers: it is what links a Google
    -- account and a generic OIDC account to one person. citext makes the
    -- uniqueness constraint case-insensitive, which matters because IdPs
    -- disagree about case normalisation.
    email         citext      NOT NULL UNIQUE,
    display_name  text        NOT NULL DEFAULT '',
    avatar_url    text,

    -- Stable subject identifiers from each identity provider. These, not
    -- email, are the primary lookup key: an IdP may let a user change their
    -- email address, and the subject survives that.
    google_sub    text        UNIQUE,
    oidc_sub      text        UNIQUE,

    -- Only set for local accounts, which are off by default and intended for
    -- development or a break-glass administrator.
    password_hash text,

    role          user_role   NOT NULL DEFAULT 'user',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    -- Disabled rather than deleted: play history and playlists reference
    -- users, and revoking access should not erase library metadata.
    disabled_at   timestamptz,

    -- An account with no credential can never be signed into, so it is always
    -- a bug rather than a state worth representing.
    CONSTRAINT users_has_credential CHECK (
        google_sub IS NOT NULL OR oidc_sub IS NOT NULL OR password_hash IS NOT NULL
    )
);

CREATE TABLE sessions (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- SHA-256 of the bearer token. The token is 256 bits of CSPRNG output, so
    -- it needs no slow hash: there is nothing to brute-force offline. Storing
    -- the digest means a database leak does not hand over live sessions.
    token_hash   bytea       NOT NULL UNIQUE,
    csrf_token   text        NOT NULL,
    user_agent   text,
    ip           inet,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);

CREATE INDEX sessions_user_id_idx ON sessions (user_id);
-- Supports the expiry sweep.
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- +goose Down
DROP TABLE sessions;
DROP TABLE users;
DROP TYPE user_role;
