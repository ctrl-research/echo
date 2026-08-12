-- name: UpsertYTItem :one
INSERT INTO yt_items (video_id, title, uploader, duration_ms, thumbnail_url)
VALUES ($1, $2, $3, sqlc.narg(duration_ms)::int, sqlc.narg(thumbnail_url)::text)
ON CONFLICT (video_id) DO UPDATE
SET title      = COALESCE(NULLIF(EXCLUDED.title, ''), yt_items.title),
    uploader   = COALESCE(NULLIF(EXCLUDED.uploader, ''), yt_items.uploader),
    updated_at = now()
RETURNING *;

-- name: GetYTItem :one
SELECT * FROM yt_items WHERE video_id = $1;

-- name: MarkYTDownloading :exec
UPDATE yt_items
SET state = 'downloading', error = NULL, updated_at = now()
WHERE video_id = $1;

-- Both timestamps are computed server-side, for the same reason job scheduling
-- is: the database clock is the only one that matters.
-- name: MarkYTReady :one
UPDATE yt_items
SET state = 'ready',
    blob_key = sqlc.arg(blob_key)::text,
    bytes = sqlc.arg(bytes)::bigint,
    title = COALESCE(NULLIF(sqlc.arg(title)::text, ''), title),
    uploader = COALESCE(NULLIF(sqlc.arg(uploader)::text, ''), uploader),
    duration_ms = COALESCE(sqlc.narg(duration_ms)::int, duration_ms),
    cached_at = now(),
    last_accessed_at = now(),
    expires_at = now() + sqlc.arg(ttl)::interval,
    error = NULL,
    updated_at = now()
WHERE video_id = sqlc.arg(video_id)
RETURNING *;

-- name: MarkYTFailed :exec
UPDATE yt_items
SET state = 'failed', error = sqlc.arg(error)::text, updated_at = now()
WHERE video_id = sqlc.arg(video_id);

-- Sliding window: every play pushes the expiry out, capped so a single
-- much-played item cannot hold cache space forever.
-- name: TouchYTItem :exec
UPDATE yt_items
SET last_accessed_at = now(),
    expires_at = LEAST(
        now() + sqlc.arg(ttl)::interval,
        cached_at + sqlc.arg(max_lifetime)::interval
    ),
    updated_at = now()
WHERE video_id = sqlc.arg(video_id) AND state = 'ready';

-- ---- eviction ------------------------------------------------------------------

-- Promoted items are excluded everywhere in this section: their bytes live
-- under a library root now, not in the disposable cache.

-- name: ExpiredYTItems :many
SELECT * FROM yt_items
WHERE state = 'ready' AND promoted_track_id IS NULL AND expires_at <= now();

-- Least recently used first, which is the order to reclaim space in.
-- name: YTItemsByLRU :many
SELECT * FROM yt_items
WHERE state = 'ready' AND promoted_track_id IS NULL
ORDER BY last_accessed_at
LIMIT $1;

-- name: YTCacheBytes :one
SELECT COALESCE(sum(bytes), 0)::bigint
FROM yt_items
WHERE state = 'ready' AND promoted_track_id IS NULL;

-- name: MarkYTEvicted :exec
UPDATE yt_items
SET state = 'evicted', blob_key = NULL, bytes = NULL,
    cached_at = NULL, expires_at = NULL, updated_at = now()
WHERE video_id = $1;

-- ---- promotion -----------------------------------------------------------------

-- name: MarkYTPromoted :exec
UPDATE yt_items
SET promoted_track_id = $2, expires_at = NULL, updated_at = now()
WHERE video_id = $1;

-- name: ListYTItems :many
SELECT y.*, t.id AS track_id
FROM yt_items y
LEFT JOIN tracks t ON t.id = y.promoted_track_id
WHERE y.state <> 'evicted'
ORDER BY y.updated_at DESC
LIMIT $1;
