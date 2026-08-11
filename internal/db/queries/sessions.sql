-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, csrf_token, user_agent, ip, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- Resolves a bearer token to its session and owner in one round trip, and
-- filters out expired sessions and disabled accounts so callers cannot forget
-- to. Returning no row is the only "not authenticated" signal handlers need.
-- name: GetSessionByTokenHash :one
SELECT sqlc.embed(sessions), sqlc.embed(users)
FROM sessions
JOIN users ON users.id = sessions.user_id
WHERE sessions.token_hash = $1
  AND sessions.expires_at > now()
  AND users.disabled_at IS NULL;

-- Throttled by the caller to avoid a write on every request.
-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = now() WHERE id = $1;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = $1;

-- name: DeleteSessionsForUser :exec
DELETE FROM sessions WHERE user_id = $1;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at <= now();
