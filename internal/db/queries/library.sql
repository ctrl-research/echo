-- ---- roots -----------------------------------------------------------------

-- name: UpsertLibraryRoot :one
INSERT INTO library_roots (path, writable)
VALUES ($1, $2)
ON CONFLICT (path) DO UPDATE SET writable = EXCLUDED.writable
RETURNING *;

-- name: ListLibraryRoots :many
SELECT * FROM library_roots WHERE enabled ORDER BY path;

-- name: GetLibraryRoot :one
SELECT * FROM library_roots WHERE id = $1;

-- name: MarkScanStarted :exec
UPDATE library_roots
SET last_scan_started_at = now(), last_scan_error = NULL
WHERE id = $1;

-- name: MarkScanFinished :exec
UPDATE library_roots
SET last_scan_finished_at = now(), last_scan_error = sqlc.narg(error)::text
WHERE id = $1;

-- ---- reconciliation ---------------------------------------------------------

-- Reconciliation is by normalised name, so "The Beatles" and "Beatles" collapse
-- to one artist. ON CONFLICT DO UPDATE rather than DO NOTHING because a bare
-- DO NOTHING returns no row on conflict, which would cost a second round trip
-- on the overwhelmingly common already-exists path.

-- Variants that normalise together share a row, so one of them has to supply
-- the display name. Keeping whichever inserted first makes that a race between
-- scanner workers — the same library could show "The Beatles" or "Beatles"
-- depending on file order. Preferring the longer form is both deterministic and
-- usually the more complete name.
-- name: UpsertArtist :one
INSERT INTO artists (name, norm_name, sort_name)
VALUES ($1, $2, sqlc.narg(sort_name)::text)
ON CONFLICT (norm_name) DO UPDATE
SET name = CASE WHEN length(EXCLUDED.name) > length(artists.name)
                THEN EXCLUDED.name ELSE artists.name END
RETURNING *;

-- name: UpsertAlbum :one
INSERT INTO albums (name, norm_name, album_artist_id, year, cover_art_id)
VALUES ($1, $2, sqlc.narg(album_artist_id)::uuid, sqlc.narg(year)::int, sqlc.narg(cover_art_id)::uuid)
ON CONFLICT (norm_name, album_artist_id) DO UPDATE
SET name         = CASE WHEN length(EXCLUDED.name) > length(albums.name)
                        THEN EXCLUDED.name ELSE albums.name END,
    year         = COALESCE(albums.year, EXCLUDED.year),
    cover_art_id = COALESCE(albums.cover_art_id, EXCLUDED.cover_art_id)
RETURNING *;

-- name: UpsertGenre :one
INSERT INTO genres (name) VALUES ($1)
ON CONFLICT (name) DO UPDATE SET name = genres.name
RETURNING *;

-- name: UpsertCoverArt :one
INSERT INTO cover_art (hash, blob_key, source, mime, width, height, bytes)
VALUES ($1, $2, $3, $4, sqlc.narg(width)::int, sqlc.narg(height)::int, $5)
ON CONFLICT (hash) DO UPDATE SET blob_key = cover_art.blob_key
RETURNING *;

-- ---- tracks -----------------------------------------------------------------

-- Returns just enough to decide whether a file changed, without loading whole
-- rows for a library that is mostly unchanged.
-- name: ListTrackStatsForRoot :many
SELECT id, rel_path, size, mtime FROM tracks WHERE root_id = $1;

-- name: GetTrackByPath :one
SELECT * FROM tracks WHERE root_id = $1 AND rel_path = $2;

-- Move detection: a file whose content hash matches a row that is now missing,
-- or whose recorded path no longer exists, is the same track relocated.
-- name: FindTrackByHash :one
SELECT * FROM tracks
WHERE content_hash = $1 AND root_id = $2
ORDER BY missing_at NULLS LAST
LIMIT 1;

-- name: UpsertTrack :one
INSERT INTO tracks (
    root_id, rel_path, size, mtime, content_hash,
    duration_ms, bitrate, sample_rate, channels, codec, suffix,
    title, track_no, disc_no, year,
    album_id, artist_id, album_artist_id, cover_art_id, missing_at
) VALUES (
    $1, $2, $3, $4, $5,
    sqlc.narg(duration_ms)::int, sqlc.narg(bitrate)::int, sqlc.narg(sample_rate)::int,
    sqlc.narg(channels)::smallint, sqlc.narg(codec)::text, $6,
    $7, sqlc.narg(track_no)::int, sqlc.narg(disc_no)::int, sqlc.narg(year)::int,
    sqlc.narg(album_id)::uuid, sqlc.narg(artist_id)::uuid,
    sqlc.narg(album_artist_id)::uuid, sqlc.narg(cover_art_id)::uuid, NULL
)
ON CONFLICT (root_id, rel_path) DO UPDATE SET
    size = EXCLUDED.size, mtime = EXCLUDED.mtime, content_hash = EXCLUDED.content_hash,
    duration_ms = EXCLUDED.duration_ms, bitrate = EXCLUDED.bitrate,
    sample_rate = EXCLUDED.sample_rate, channels = EXCLUDED.channels,
    codec = EXCLUDED.codec, suffix = EXCLUDED.suffix,
    title = EXCLUDED.title, track_no = EXCLUDED.track_no,
    disc_no = EXCLUDED.disc_no, year = EXCLUDED.year,
    album_id = EXCLUDED.album_id, artist_id = EXCLUDED.artist_id,
    album_artist_id = EXCLUDED.album_artist_id, cover_art_id = EXCLUDED.cover_art_id,
    missing_at = NULL,
    updated_at = now()
RETURNING *;

-- Relocating a track keeps its id, and therefore its playlist entries and play
-- history, across a library reorganisation.
-- name: MoveTrack :one
UPDATE tracks
SET rel_path = sqlc.arg(rel_path)::text,
    size = sqlc.arg(size)::bigint,
    mtime = sqlc.arg(mtime)::timestamptz,
    missing_at = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: MarkTracksMissing :execrows
UPDATE tracks SET missing_at = now(), updated_at = now()
WHERE id = ANY(sqlc.arg(ids)::uuid[]) AND missing_at IS NULL;

-- Cutoff computed server-side, for the same reason run_after is: the database
-- clock is the only one that matters here.
-- name: DeleteMissingTracksOlderThan :execrows
DELETE FROM tracks
WHERE missing_at IS NOT NULL AND missing_at < now() - sqlc.arg(older_than)::interval;

-- name: ReplaceTrackGenres :exec
WITH cleared AS (
    DELETE FROM track_genres WHERE track_id = sqlc.arg(track_id)
)
INSERT INTO track_genres (track_id, genre_id)
SELECT sqlc.arg(track_id), unnest(sqlc.arg(genre_ids)::uuid[])
ON CONFLICT DO NOTHING;

-- Written in the same transaction as the track, from the effective values.
-- name: UpsertTrackSearch :exec
INSERT INTO track_search (track_id, haystack)
VALUES ($1, $2)
ON CONFLICT (track_id) DO UPDATE SET haystack = EXCLUDED.haystack;

-- Rows that no track references any more. Cover art is content-addressed and
-- shared, so it can only be removed once nothing points at it.
-- name: DeleteOrphanedCoverArt :many
DELETE FROM cover_art
WHERE NOT EXISTS (SELECT 1 FROM tracks WHERE tracks.cover_art_id = cover_art.id)
  AND NOT EXISTS (SELECT 1 FROM albums WHERE albums.cover_art_id = cover_art.id)
RETURNING blob_key;

-- Albums and artists are swept in two statements, deliberately. A single
-- statement with a CTE cannot work: every part of it sees the same snapshot, so
-- the artist check would still see the albums the CTE is deleting and spare
-- every artist whose only album just went away.
-- name: DeleteOrphanedAlbums :execrows
DELETE FROM albums
WHERE NOT EXISTS (SELECT 1 FROM tracks WHERE tracks.album_id = albums.id);

-- name: DeleteOrphanedArtists :execrows
DELETE FROM artists
WHERE NOT EXISTS (SELECT 1 FROM tracks WHERE tracks.artist_id = artists.id)
  AND NOT EXISTS (SELECT 1 FROM tracks WHERE tracks.album_artist_id = artists.id)
  AND NOT EXISTS (SELECT 1 FROM albums WHERE albums.album_artist_id = artists.id);

-- ---- counts -----------------------------------------------------------------

-- name: CountTracks :one
SELECT count(*) FROM tracks WHERE missing_at IS NULL;

-- name: LibraryStats :one
SELECT
    (SELECT count(*) FROM tracks WHERE missing_at IS NULL)  AS tracks,
    (SELECT count(*) FROM tracks WHERE missing_at IS NOT NULL) AS missing,
    (SELECT count(*) FROM albums)                            AS albums,
    (SELECT count(*) FROM artists)                           AS artists,
    (SELECT count(*) FROM genres)                            AS genres,
    (SELECT COALESCE(sum(duration_ms), 0) FROM tracks WHERE missing_at IS NULL) AS total_duration_ms;
