// Package media serves audio bytes and cover art.
//
// The fast path is deliberately boring: open the original file and hand it to
// http.ServeContent, which implements Range, If-Range, If-Modified-Since,
// ETags, and multipart ranges correctly. Seeking in a browser is entirely a
// property of getting those right, and reimplementing them is a reliable way to
// produce audio that plays but cannot be scrubbed.
package media

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jonathanng/echo/internal/blobstore"
	"github.com/jonathanng/echo/internal/db/dbgen"
)

// nativeFormats play in every browser Echo targets, so they are streamed
// untouched. Everything else needs ffmpeg.
//
// FLAC is on this list deliberately: Chrome, Firefox, and Safari 11+ all decode
// it, and transcoding it would throw away the quality that is the reason to
// keep FLAC at all.
var nativeFormats = map[string]string{
	"mp3":  "audio/mpeg",
	"m4a":  "audio/mp4",
	"m4b":  "audio/mp4",
	"mp4":  "audio/mp4",
	"aac":  "audio/aac",
	"flac": "audio/flac",
	"ogg":  "audio/ogg",
	"oga":  "audio/ogg",
	"opus": "audio/ogg",
	"wav":  "audio/wav",
}

// ErrNotFound means the track is unknown, or its file has gone missing.
var ErrNotFound = errors.New("media: track not found")

// ErrUnsupported means the file needs transcoding and ffmpeg is unavailable.
var ErrUnsupported = errors.New("media: format needs transcoding but ffmpeg is not available")

type Service struct {
	q         *dbgen.Queries
	blobs     blobstore.Store
	log       *slog.Logger
	transcode *Transcoder
}

func NewService(pool *pgxpool.Pool, blobs blobstore.Store, log *slog.Logger) *Service {
	return &Service{
		q:         dbgen.New(pool),
		blobs:     blobs,
		log:       log,
		transcode: NewTranscoder(blobs, log),
	}
}

// TranscodingAvailable reports whether ffmpeg was found at startup.
func (s *Service) TranscodingAvailable() bool { return s.transcode.Available() }

// ServeTrack writes one track to w, honouring range requests.
func (s *Service) ServeTrack(w http.ResponseWriter, r *http.Request, trackID uuid.UUID) error {
	row, err := s.q.GetTrackForStream(r.Context(), trackID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("load track: %w", err)
	}

	suffix := strings.ToLower(row.Suffix)
	if mime, native := nativeFormats[suffix]; native {
		return s.serveOriginal(w, r, row, mime)
	}
	return s.serveTranscoded(w, r, row)
}

func (s *Service) serveOriginal(w http.ResponseWriter, r *http.Request, row dbgen.GetTrackForStreamRow, mime string) error {
	path, err := resolveTrackPath(row.RootPath, row.RelPath)
	if err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// On disk a moment ago, gone now. The next scan will mark it.
			s.log.Warn("track file missing at stream time", "path", row.RelPath)
			return ErrNotFound
		}
		return fmt.Errorf("open track: %w", err)
	}
	defer f.Close()

	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=31536000")
	w.Header().Set("Accept-Ranges", "bytes")
	// ServeContent honours If-None-Match and If-Range but does not invent an
	// ETag, so one has to be supplied or every re-listen refetches the whole
	// track. The scanner's content hash is exactly the right value: stable
	// across restarts, and different the moment the file changes.
	w.Header().Set("ETag", etagFor(row.ContentHash, suffixOf(row.RelPath)))

	// ServeContent handles the conditional and range machinery; the name is
	// only used for content-type sniffing, which the header above overrides.
	http.ServeContent(w, r, filepath.Base(path), row.Mtime, f)
	return nil
}

// serveTranscoded converts on first request and serves from cache thereafter.
//
// Piping ffmpeg straight to the response would make the track unseekable: the
// length is unknown and the stream cannot be rewound. Paying once to produce a
// real file keeps every later request on the same boring ServeContent path.
func (s *Service) serveTranscoded(w http.ResponseWriter, r *http.Request, row dbgen.GetTrackForStreamRow) error {
	if !s.transcode.Available() {
		return ErrUnsupported
	}
	source, err := resolveTrackPath(row.RootPath, row.RelPath)
	if err != nil {
		return err
	}

	key, err := s.transcode.Ensure(r.Context(), row.ID, source)
	if err != nil {
		return err
	}

	// A local backing file lets the kernel do the work; object storage would
	// fall back to the reader.
	if path, ok := s.blobs.LocalPath(key); ok {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open transcode: %w", err)
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", transcodeMIME)
		w.Header().Set("Cache-Control", "private, max-age=31536000")
		w.Header().Set("Accept-Ranges", "bytes")
		// Distinct from the original's ETag: same source, different bytes.
		w.Header().Set("ETag", etagFor(row.ContentHash, "opus"))
		http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
		return nil
	}

	reader, info, err := s.blobs.Open(r.Context(), key)
	if err != nil {
		return err
	}
	defer reader.Close()
	w.Header().Set("Content-Type", transcodeMIME)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", etagFor(row.ContentHash, "opus"))
	http.ServeContent(w, r, key, info.ModTime, reader)
	return nil
}

// ServeBlobAudio streams audio that lives in the blob store rather than under a
// library root — YouTube's cache. Shares the ServeContent path, so range
// requests and seeking behave identically to a library track.
func (s *Service) ServeBlobAudio(w http.ResponseWriter, r *http.Request, key string) error {
	if path, ok := s.blobs.LocalPath(key); ok {
		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ErrNotFound
			}
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		writeAudioHeaders(w, transcodeMIME, `"`+key+`"`)
		http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
		return nil
	}

	reader, info, err := s.blobs.Open(r.Context(), key)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	defer reader.Close()
	writeAudioHeaders(w, transcodeMIME, `"`+key+`"`)
	http.ServeContent(w, r, key, info.ModTime, reader)
	return nil
}

func writeAudioHeaders(w http.ResponseWriter, mime, etag string) {
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=31536000")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", etag)
}

// ServeCoverArt writes a cover image, by cover-art id.
func (s *Service) ServeCoverArt(w http.ResponseWriter, r *http.Request, artID uuid.UUID) error {
	row, err := s.q.GetCoverArt(r.Context(), artID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return s.serveBlob(w, r, row.BlobKey, row.Mime)
}

// ServeAlbumCoverArt writes an album's cover, saving the client a lookup.
func (s *Service) ServeAlbumCoverArt(w http.ResponseWriter, r *http.Request, albumID uuid.UUID) error {
	row, err := s.q.GetAlbumCoverArt(r.Context(), albumID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return s.serveBlob(w, r, row.BlobKey, row.Mime)
}

func (s *Service) serveBlob(w http.ResponseWriter, r *http.Request, key, mime string) error {
	reader, info, err := s.blobs.Open(r.Context(), key)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	defer reader.Close()

	w.Header().Set("Content-Type", mime)
	// Content-addressed by hash, so the bytes behind an id never change.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, key, info.ModTime, reader)
	return nil
}

// resolveTrackPath joins a root and a relative path, refusing anything that
// escapes the root.
//
// rel_path is written by the scanner rather than by a user, so this should be
// unreachable — which is exactly why it is worth asserting: a future bug that
// lets a crafted path into that column must not become arbitrary file read.
func resolveTrackPath(root, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("media: relative path escapes root: %q", rel)
	}
	full := filepath.Join(root, clean)
	if !strings.HasPrefix(full, filepath.Clean(root)+string(os.PathSeparator)) {
		return "", fmt.Errorf("media: resolved path escapes root: %q", rel)
	}
	return full, nil
}

// etagFor builds a strong validator from the scanner's content hash. The
// variant distinguishes the original bytes from a transcode of them.
func etagFor(contentHash []byte, variant string) string {
	return `"` + hex.EncodeToString(contentHash) + "-" + variant + `"`
}

func suffixOf(relPath string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(relPath)), ".")
}

// IsNativeFormat reports whether a file suffix streams without transcoding.
func IsNativeFormat(suffix string) bool {
	_, ok := nativeFormats[strings.ToLower(strings.TrimPrefix(suffix, "."))]
	return ok
}
