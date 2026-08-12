-- ---- playlists ----------------------------------------------------------------

-- name: CreatePlaylist :one
INSERT INTO playlists (user_id, name, description, public)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- Own playlists, plus anyone's public ones. Ownership is returned so the client
-- can tell which are editable without a second lookup.
-- name: ListPlaylists :many
SELECT p.*,
       (p.user_id = sqlc.arg(user_id)) AS owned,
       u.display_name AS owner_name,
       count(pt.id)::bigint AS track_count,
       COALESCE(sum(t.duration_ms), 0)::bigint AS duration_ms
FROM playlists p
JOIN users u ON u.id = p.user_id
LEFT JOIN playlist_tracks pt ON pt.playlist_id = p.id
LEFT JOIN tracks t ON t.id = pt.track_id AND t.missing_at IS NULL
WHERE p.user_id = sqlc.arg(user_id) OR p.public
GROUP BY p.id, u.display_name
ORDER BY lower(p.name), p.id;

-- name: GetPlaylist :one
SELECT p.*,
       (p.user_id = sqlc.arg(user_id)) AS owned,
       u.display_name AS owner_name,
       count(pt.id)::bigint AS track_count,
       COALESCE(sum(t.duration_ms), 0)::bigint AS duration_ms
FROM playlists p
JOIN users u ON u.id = p.user_id
LEFT JOIN playlist_tracks pt ON pt.playlist_id = p.id
LEFT JOIN tracks t ON t.id = pt.track_id AND t.missing_at IS NULL
WHERE p.id = sqlc.arg(id) AND (p.user_id = sqlc.arg(user_id) OR p.public)
GROUP BY p.id, u.display_name;

-- Scoped by user_id so a non-owner's update silently matches nothing rather
-- than needing a separate ownership check that a caller could forget.
-- name: UpdatePlaylist :one
UPDATE playlists
SET name        = COALESCE(sqlc.narg(name)::text, name),
    description = COALESCE(sqlc.narg(description)::text, description),
    public      = COALESCE(sqlc.narg(public)::bool, public),
    updated_at  = now()
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id)
RETURNING *;

-- name: DeletePlaylist :execrows
DELETE FROM playlists WHERE id = $1 AND user_id = $2;

-- name: PlaylistIsOwnedBy :one
SELECT EXISTS (SELECT 1 FROM playlists WHERE id = $1 AND user_id = $2);

-- ---- playlist contents ----------------------------------------------------------

-- Missing tracks are kept in the listing rather than filtered out: a playlist
-- entry pointing at an unmounted drive should show as unavailable, not silently
-- vanish and leave the owner wondering what they deleted.
-- name: ListPlaylistTracks :many
SELECT pt.id AS entry_id, pt.position, pt.added_at,
       te.*,
       (te.missing_at IS NOT NULL)::bool AS unavailable,
       (SELECT COALESCE(array_agg(g.name ORDER BY g.name), '{}')
        FROM track_genres tg JOIN genres g ON g.id = tg.genre_id
        WHERE tg.track_id = te.id)::text[] AS genres,
       EXISTS (SELECT 1 FROM favorites f
               WHERE f.user_id = sqlc.arg(user_id) AND f.entity_type = 'track'
                 AND f.entity_id = te.id) AS favorite
FROM playlist_tracks pt
JOIN tracks_effective te ON te.id = pt.track_id
WHERE pt.playlist_id = sqlc.arg(playlist_id)
ORDER BY pt.position, pt.id;

-- Appends at the end. The position is computed in the same statement so two
-- concurrent adds cannot both read the same max and collide.
-- name: AppendPlaylistTrack :one
INSERT INTO playlist_tracks (playlist_id, track_id, position)
SELECT sqlc.arg(playlist_id), sqlc.arg(track_id),
       COALESCE(max(existing.position), -1) + 1
FROM playlist_tracks existing
WHERE existing.playlist_id = sqlc.arg(playlist_id)
RETURNING *;

-- name: RemovePlaylistTrack :execrows
DELETE FROM playlist_tracks WHERE id = $1 AND playlist_id = $2;

-- Renumbers from zero after a removal, so positions stay dense and the next
-- append lands where it should.
-- name: CompactPlaylistPositions :exec
WITH ordered AS (
    SELECT src.id,
           row_number() OVER (ORDER BY src.position, src.id) - 1 AS new_position
    FROM playlist_tracks src WHERE src.playlist_id = $1
)
UPDATE playlist_tracks pt
SET position = ordered.new_position
FROM ordered
WHERE pt.id = ordered.id AND pt.position <> ordered.new_position;

-- Applies a whole new order in one statement. The unique constraint on
-- (playlist_id, position) is deferrable precisely so this can pass through
-- intermediate states where two rows briefly share a position.
-- name: ReorderPlaylist :exec
UPDATE playlist_tracks pt
SET position = new_order.position
FROM (
    SELECT unnest(sqlc.arg(entry_ids)::uuid[]) AS id,
           generate_subscripts(sqlc.arg(entry_ids)::uuid[], 1) - 1 AS position
) AS new_order
WHERE pt.id = new_order.id AND pt.playlist_id = sqlc.arg(playlist_id);

-- name: CountPlaylistTracks :one
SELECT count(*) FROM playlist_tracks WHERE playlist_id = $1;

-- ---- favorites ------------------------------------------------------------------

-- name: AddFavorite :exec
INSERT INTO favorites (user_id, entity_type, entity_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: RemoveFavorite :execrows
DELETE FROM favorites WHERE user_id = $1 AND entity_type = $2 AND entity_id = $3;

-- name: ListFavoriteTracks :many
SELECT te.*,
       (SELECT COALESCE(array_agg(g.name ORDER BY g.name), '{}')
        FROM track_genres tg JOIN genres g ON g.id = tg.genre_id
        WHERE tg.track_id = te.id)::text[] AS genres,
       f.created_at AS favorited_at
FROM favorites f
JOIN tracks_effective te ON te.id = f.entity_id
WHERE f.user_id = $1 AND f.entity_type = 'track' AND te.missing_at IS NULL
ORDER BY f.created_at DESC
LIMIT $2;

-- name: IsFavorite :one
SELECT EXISTS (
    SELECT 1 FROM favorites
    WHERE user_id = $1 AND entity_type = $2 AND entity_id = $3
);

-- ---- plays ------------------------------------------------------------------------

-- name: RecordPlay :one
INSERT INTO plays (user_id, track_id, ms_played, source)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListHistory :many
SELECT p.id, p.played_at, p.ms_played, p.source,
       te.id AS track_id, te.title, te.artist_name, te.album_name,
       te.album_id, te.cover_art_id, te.duration_ms
FROM plays p
LEFT JOIN tracks_effective te ON te.id = p.track_id
WHERE p.user_id = $1
ORDER BY p.played_at DESC
LIMIT $2;

-- name: TrackPlayStats :one
SELECT count(*)::bigint AS play_count, max(played_at) AS last_played_at
FROM plays WHERE user_id = $1 AND track_id = $2;

-- name: TopTracks :many
SELECT te.*,
       (SELECT COALESCE(array_agg(g.name ORDER BY g.name), '{}')
        FROM track_genres tg JOIN genres g ON g.id = tg.genre_id
        WHERE tg.track_id = te.id)::text[] AS genres,
       count(p.id)::bigint AS play_count
FROM plays p
JOIN tracks_effective te ON te.id = p.track_id
WHERE p.user_id = $1 AND te.missing_at IS NULL
GROUP BY te.id, te.root_id, te.rel_path, te.size, te.mtime, te.content_hash,
         te.duration_ms, te.bitrate, te.sample_rate, te.channels, te.codec,
         te.suffix, te.album_id, te.artist_id, te.album_artist_id,
         te.cover_art_id, te.missing_at, te.created_at, te.updated_at,
         te.title, te.track_no, te.disc_no, te.year, te.artist_name,
         te.album_name, te.album_artist_name
ORDER BY play_count DESC, lower(te.title)
LIMIT $2;

-- Duration is needed server-side to decide whether a reported play qualifies;
-- trusting the client's number would let anyone inflate their own counts.
-- name: GetTrackDuration :one
SELECT duration_ms FROM tracks WHERE id = $1 AND missing_at IS NULL;
