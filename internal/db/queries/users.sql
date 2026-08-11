-- name: CreateUser :one
INSERT INTO users (email, display_name, avatar_url, google_sub, oidc_sub, password_hash, role)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- Subject lookups come first in the sign-in path: an IdP subject is stable
-- even when the user changes their email address at the provider.
-- name: GetUserByGoogleSub :one
SELECT * FROM users WHERE google_sub = $1;

-- name: GetUserByOIDCSub :one
SELECT * FROM users WHERE oidc_sub = $1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY email;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- Attaches a provider subject to an account found by email, so that signing in
-- with Google and then with a self-hosted IdP lands on one account rather than
-- creating a second. Only fills an empty column: a subject already recorded is
-- left alone so a different provider account cannot claim an existing user.
-- name: LinkGoogleSub :one
UPDATE users
SET google_sub   = COALESCE(google_sub, sqlc.arg(google_sub)::text),
    display_name = CASE WHEN display_name = '' THEN sqlc.arg(display_name)::text
                        ELSE display_name END,
    avatar_url   = COALESCE(sqlc.narg(avatar_url)::text, avatar_url),
    updated_at   = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: LinkOIDCSub :one
UPDATE users
SET oidc_sub     = COALESCE(oidc_sub, sqlc.arg(oidc_sub)::text),
    display_name = CASE WHEN display_name = '' THEN sqlc.arg(display_name)::text
                        ELSE display_name END,
    avatar_url   = COALESCE(sqlc.narg(avatar_url)::text, avatar_url),
    updated_at   = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- Refreshes the profile fields an IdP owns on every sign-in, so a renamed or
-- re-avatared account stays current without a separate sync.
-- name: UpdateProfile :one
UPDATE users
SET display_name = CASE WHEN sqlc.arg(display_name)::text <> '' THEN sqlc.arg(display_name)::text
                        ELSE display_name END,
    avatar_url   = COALESCE(sqlc.narg(avatar_url)::text, avatar_url),
    updated_at   = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- Partial update for the admin API.
--
-- password_hash and role use COALESCE with a nullable parameter, so NULL means
-- "leave unchanged". A boolean flag with a non-nullable parameter cannot work
-- for role: Postgres coerces the bound value to user_role while planning, so
-- an unused ''::user_role fails even inside a CASE branch that is never taken.
--
-- disabled_at genuinely needs a flag, because NULL is a meaningful target
-- value there — it is how an account gets re-enabled.
-- name: UpdateUser :one
UPDATE users
SET password_hash = COALESCE(sqlc.narg(password_hash)::text, password_hash),
    role          = COALESCE(sqlc.narg(role)::user_role, role),
    disabled_at   = CASE WHEN sqlc.arg(set_disabled)::bool
                         THEN sqlc.narg(disabled_at)::timestamptz ELSE disabled_at END,
    updated_at    = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteUser :execrows
DELETE FROM users WHERE id = $1;

-- Guards the last-admin check. Counts only admins who can still sign in.
-- name: CountActiveAdmins :one
SELECT count(*) FROM users
WHERE role = 'admin' AND disabled_at IS NULL;
