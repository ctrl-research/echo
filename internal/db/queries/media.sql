-- Everything needed to serve one track's bytes, in a single round trip: the
-- root path and relative path that locate the file, plus the metadata the
-- response headers need.
-- name: GetTrackForStream :one
SELECT t.id, t.rel_path, t.suffix, t.size, t.mtime, t.duration_ms, t.codec,
       t.content_hash, r.path AS root_path
FROM tracks t
JOIN library_roots r ON r.id = t.root_id
WHERE t.id = $1 AND t.missing_at IS NULL;

-- name: GetCoverArt :one
SELECT id, blob_key, mime, bytes FROM cover_art WHERE id = $1;

-- Album art is requested by album far more often than by its own id, so this
-- saves the client a lookup it would otherwise always have to make first.
-- name: GetAlbumCoverArt :one
SELECT ca.id, ca.blob_key, ca.mime, ca.bytes
FROM albums al
JOIN cover_art ca ON ca.id = al.cover_art_id
WHERE al.id = $1;
