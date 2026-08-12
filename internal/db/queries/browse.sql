-- Read paths for browsing and searching the library.
--
-- Every listing is keyset-paginated on (sort key, id). Offset pagination
-- degrades badly past a few thousand rows and double-serves items when the
-- library changes mid-scroll, which it does constantly while a scan runs.

-- ---- tracks -------------------------------------------------------------------

-- The one query behind GET /tracks. Each filter is inert when its parameter is
-- NULL, so one statement serves every combination the API allows rather than a
-- combinatorial set of near-identical queries.
--
-- The cursor predicate is a row comparison rather than OR-ed inequalities:
-- (sort_key, id) > (last_sort_key, last_id) uses the composite index directly,
-- where the expanded form usually will not.
-- name: ListTracks :many
SELECT te.*,
       (SELECT COALESCE(array_agg(g.name ORDER BY g.name), '{}')
        FROM track_genres tg JOIN genres g ON g.id = tg.genre_id
        WHERE tg.track_id = te.id)::text[] AS genres
FROM tracks_effective te
WHERE te.missing_at IS NULL
  AND (sqlc.narg(artist_id)::uuid IS NULL
       OR te.artist_id = sqlc.narg(artist_id)::uuid
       OR te.album_artist_id = sqlc.narg(artist_id)::uuid)
  AND (sqlc.narg(album_id)::uuid IS NULL OR te.album_id = sqlc.narg(album_id)::uuid)
  AND (sqlc.narg(year)::int IS NULL OR te.year = sqlc.narg(year)::int)
  AND (sqlc.narg(genre)::text IS NULL OR EXISTS (
        SELECT 1 FROM track_genres tg JOIN genres g ON g.id = tg.genre_id
        WHERE tg.track_id = te.id AND g.name = sqlc.narg(genre)::citext))
  AND (sqlc.narg(cursor_sort)::text IS NULL
       OR (lower(te.title), te.id) > (sqlc.narg(cursor_sort)::text, sqlc.narg(cursor_id)::uuid))
ORDER BY lower(te.title), te.id
LIMIT $1;

-- name: GetTrack :one
SELECT te.*,
       (SELECT COALESCE(array_agg(g.name ORDER BY g.name), '{}')
        FROM track_genres tg JOIN genres g ON g.id = tg.genre_id
        WHERE tg.track_id = te.id)::text[] AS genres
FROM tracks_effective te
WHERE te.id = $1;

-- ---- albums -------------------------------------------------------------------

-- name: ListAlbums :many
SELECT al.id, al.name, al.year, al.cover_art_id,
       ar.id AS artist_id, COALESCE(ar.name, '')::text AS artist_name,
       count(t.id)::bigint AS track_count,
       COALESCE(sum(t.duration_ms), 0)::bigint AS duration_ms
FROM albums al
LEFT JOIN artists ar ON ar.id = al.album_artist_id
JOIN tracks t ON t.album_id = al.id AND t.missing_at IS NULL
WHERE (sqlc.narg(artist_id)::uuid IS NULL OR al.album_artist_id = sqlc.narg(artist_id)::uuid)
  AND (sqlc.narg(year)::int IS NULL OR al.year = sqlc.narg(year)::int)
  AND (sqlc.narg(genre)::text IS NULL OR EXISTS (
        SELECT 1 FROM track_genres tg JOIN genres g ON g.id = tg.genre_id
        WHERE tg.track_id = t.id AND g.name = sqlc.narg(genre)::citext))
  AND (sqlc.narg(cursor_sort)::text IS NULL
       OR (lower(al.name), al.id) > (sqlc.narg(cursor_sort)::text, sqlc.narg(cursor_id)::uuid))
GROUP BY al.id, al.name, al.year, al.cover_art_id, ar.id, ar.name
ORDER BY lower(al.name), al.id
LIMIT $1;

-- name: GetAlbum :one
SELECT al.id, al.name, al.year, al.cover_art_id,
       ar.id AS artist_id, COALESCE(ar.name, '')::text AS artist_name,
       count(t.id)::bigint AS track_count,
       COALESCE(sum(t.duration_ms), 0)::bigint AS duration_ms
FROM albums al
LEFT JOIN artists ar ON ar.id = al.album_artist_id
LEFT JOIN tracks t ON t.album_id = al.id AND t.missing_at IS NULL
WHERE al.id = $1
GROUP BY al.id, al.name, al.year, al.cover_art_id, ar.id, ar.name;

-- Disc then track number, which is the only order an album makes sense in.
-- name: ListAlbumTracks :many
SELECT te.*,
       (SELECT COALESCE(array_agg(g.name ORDER BY g.name), '{}')
        FROM track_genres tg JOIN genres g ON g.id = tg.genre_id
        WHERE tg.track_id = te.id)::text[] AS genres
FROM tracks_effective te
WHERE te.album_id = $1 AND te.missing_at IS NULL
ORDER BY COALESCE(te.disc_no, 1), COALESCE(te.track_no, 0), lower(te.title);

-- ---- artists ------------------------------------------------------------------

-- Only artists with at least one live track: reconciliation can leave rows
-- behind, and a browse list full of entries that match nothing is noise.
-- name: ListArtists :many
SELECT ar.id, ar.name,
       count(DISTINCT t.id)::bigint AS track_count,
       count(DISTINCT t.album_id)::bigint AS album_count
FROM artists ar
JOIN tracks t ON (t.artist_id = ar.id OR t.album_artist_id = ar.id) AND t.missing_at IS NULL
WHERE (sqlc.narg(cursor_sort)::text IS NULL
       OR (ar.norm_name, ar.id) > (sqlc.narg(cursor_sort)::text, sqlc.narg(cursor_id)::uuid))
GROUP BY ar.id, ar.name, ar.norm_name
ORDER BY ar.norm_name, ar.id
LIMIT $1;

-- name: GetArtist :one
SELECT ar.id, ar.name,
       count(DISTINCT t.id)::bigint AS track_count,
       count(DISTINCT t.album_id)::bigint AS album_count
FROM artists ar
LEFT JOIN tracks t ON (t.artist_id = ar.id OR t.album_artist_id = ar.id) AND t.missing_at IS NULL
WHERE ar.id = $1
GROUP BY ar.id, ar.name;

-- ---- genres -------------------------------------------------------------------

-- name: ListGenres :many
SELECT g.id, g.name::text AS name, count(t.id)::bigint AS track_count
FROM genres g
JOIN track_genres tg ON tg.genre_id = g.id
JOIN tracks t ON t.id = tg.track_id AND t.missing_at IS NULL
GROUP BY g.id, g.name
ORDER BY g.name;

-- ---- search -------------------------------------------------------------------

-- Ranked full-text search. websearch_to_tsquery accepts what a person actually
-- types — bare words, quoted phrases, "or" — without erroring on punctuation
-- the way to_tsquery does.
-- name: SearchTracksExact :many
SELECT te.*,
       (SELECT COALESCE(array_agg(g.name ORDER BY g.name), '{}')
        FROM track_genres tg JOIN genres g ON g.id = tg.genre_id
        WHERE tg.track_id = te.id)::text[] AS genres,
       ts_rank(ts.tsv, websearch_to_tsquery('simple', immutable_unaccent(sqlc.arg(query)::text))) AS rank
FROM track_search ts
JOIN tracks_effective te ON te.id = ts.track_id
WHERE te.missing_at IS NULL
  AND ts.tsv @@ websearch_to_tsquery('simple', immutable_unaccent(sqlc.arg(query)::text))
ORDER BY rank DESC, lower(te.title)
LIMIT $1;

-- Trigram fallback, for when exact matching finds too little: typos
-- ("radiohed"), partial words, and missing diacritics. Deliberately a separate
-- query rather than an OR — combining them would stop Postgres using either
-- index and turn every search into a sequential scan.
--
-- word_similarity, not similarity: the latter compares whole strings, so a
-- short query against a long haystack ("Airbag Radiohead OK Computer
-- Alternative Rock") scores far below any usable threshold and matches
-- nothing. The <% operator asks instead whether the query closely matches some
-- run of words *inside* the haystack, which is what a search box means.
-- name: SearchTracksFuzzy :many
SELECT te.*,
       (SELECT COALESCE(array_agg(g.name ORDER BY g.name), '{}')
        FROM track_genres tg JOIN genres g ON g.id = tg.genre_id
        WHERE tg.track_id = te.id)::text[] AS genres,
       word_similarity(sqlc.arg(query)::text, ts.haystack) AS rank
FROM track_search ts
JOIN tracks_effective te ON te.id = ts.track_id
WHERE te.missing_at IS NULL
  AND sqlc.arg(query)::text <% ts.haystack
ORDER BY rank DESC, lower(te.title)
LIMIT $1;

-- name: SearchAlbums :many
SELECT al.id, al.name, al.year, al.cover_art_id,
       COALESCE(ar.name, '')::text AS artist_name,
       word_similarity(sqlc.arg(query)::text, al.norm_name) AS rank
FROM albums al
LEFT JOIN artists ar ON ar.id = al.album_artist_id
WHERE EXISTS (SELECT 1 FROM tracks t WHERE t.album_id = al.id AND t.missing_at IS NULL)
  AND (sqlc.arg(query)::text <% al.norm_name OR al.norm_name ILIKE '%' || sqlc.arg(query)::text || '%')
ORDER BY rank DESC, lower(al.name)
LIMIT $1;

-- name: SearchArtists :many
SELECT ar.id, ar.name,
       word_similarity(sqlc.arg(query)::text, ar.norm_name) AS rank
FROM artists ar
WHERE EXISTS (SELECT 1 FROM tracks t
              WHERE (t.artist_id = ar.id OR t.album_artist_id = ar.id) AND t.missing_at IS NULL)
  AND (sqlc.arg(query)::text <% ar.norm_name OR ar.norm_name ILIKE '%' || sqlc.arg(query)::text || '%')
ORDER BY rank DESC, lower(ar.name)
LIMIT $1;

-- ---- overrides ----------------------------------------------------------------

-- name: UpsertTrackOverride :one
INSERT INTO track_overrides (track_id, title, artist_name, album_name,
                             album_artist_name, genre, year, track_no, disc_no)
VALUES ($1, sqlc.narg(title)::text, sqlc.narg(artist_name)::text,
        sqlc.narg(album_name)::text, sqlc.narg(album_artist_name)::text,
        sqlc.narg(genre)::text, sqlc.narg(year)::int,
        sqlc.narg(track_no)::int, sqlc.narg(disc_no)::int)
ON CONFLICT (track_id) DO UPDATE SET
    title             = COALESCE(EXCLUDED.title, track_overrides.title),
    artist_name       = COALESCE(EXCLUDED.artist_name, track_overrides.artist_name),
    album_name        = COALESCE(EXCLUDED.album_name, track_overrides.album_name),
    album_artist_name = COALESCE(EXCLUDED.album_artist_name, track_overrides.album_artist_name),
    genre             = COALESCE(EXCLUDED.genre, track_overrides.genre),
    year              = COALESCE(EXCLUDED.year, track_overrides.year),
    track_no          = COALESCE(EXCLUDED.track_no, track_overrides.track_no),
    disc_no           = COALESCE(EXCLUDED.disc_no, track_overrides.disc_no),
    updated_at        = now()
RETURNING *;

-- name: ClearTrackOverride :exec
DELETE FROM track_overrides WHERE track_id = $1;

-- Rebuilds a track's search text from its effective values.
--
-- This is the single definition of the haystack, used by both the scanner and
-- the override editor. Assembling it in Go as well would let the two drift, and
-- the symptom would be a track that is unfindable by the name shown for it.
-- name: RebuildTrackSearch :exec
INSERT INTO track_search (track_id, haystack)
SELECT te.id,
       concat_ws(' ', NULLIF(te.title, ''), NULLIF(te.artist_name, ''),
                 NULLIF(te.album_name, ''), NULLIF(te.album_artist_name, ''),
                 (SELECT string_agg(g.name::text, ' ' ORDER BY g.name)
                  FROM track_genres tg JOIN genres g ON g.id = tg.genre_id
                  WHERE tg.track_id = te.id))
FROM tracks_effective te
WHERE te.id = $1
ON CONFLICT (track_id) DO UPDATE SET haystack = EXCLUDED.haystack;
