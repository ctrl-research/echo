// Package library owns the local music collection: scanning roots, reading
// tags, reconciling artists and albums, and keeping the database in step with
// the filesystem.
//
// Library files are never written to. Metadata corrections live in
// track_overrides and are applied at read time; see docs/design.md.
package library

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jonathanng/echo/internal/blobstore"
	"github.com/jonathanng/echo/internal/db/dbgen"
)

// Scanner walks library roots and reconciles them into the database.
type Scanner struct {
	pool  *pgxpool.Pool
	q     *dbgen.Queries
	blobs blobstore.Store
	log   *slog.Logger

	// workers bounds concurrent tag reading. Probing is a mix of IO and CPU,
	// so the pool is sized to cores rather than left unbounded — 50k files
	// would otherwise open 50k descriptors at once.
	workers int
}

func NewScanner(pool *pgxpool.Pool, blobs blobstore.Store, log *slog.Logger) *Scanner {
	return &Scanner{
		pool:    pool,
		q:       dbgen.New(pool),
		blobs:   blobs,
		log:     log,
		workers: max(2, min(runtime.NumCPU(), 8)),
	}
}

// Result summarises one scan.
type Result struct {
	Seen      int
	Added     int
	Updated   int
	Moved     int
	Missing   int
	Unchanged int
	Failed    int
	Duration  time.Duration
}

func (r Result) LogArgs() []any {
	return []any{
		"seen", r.Seen, "added", r.Added, "updated", r.Updated, "moved", r.Moved,
		"missing", r.Missing, "unchanged", r.Unchanged, "failed", r.Failed,
		"duration", r.Duration.Round(time.Millisecond),
	}
}

// SyncRoots registers the configured paths, returning the persisted rows.
//
// The first writable root receives promoted YouTube downloads; every other root
// is treated as read-only regardless of its filesystem permissions.
func (s *Scanner) SyncRoots(ctx context.Context, paths []string, writablePath string) ([]dbgen.LibraryRoot, error) {
	out := make([]dbgen.LibraryRoot, 0, len(paths))
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("resolve root %q: %w", p, err)
		}
		root, err := s.q.UpsertLibraryRoot(ctx, dbgen.UpsertLibraryRootParams{
			Path:     abs,
			Writable: abs == writablePath,
		})
		if err != nil {
			return nil, fmt.Errorf("register root %q: %w", abs, err)
		}
		out = append(out, root)
	}
	return out, nil
}

// ScanRoot walks one root and brings the database in step with it.
func (s *Scanner) ScanRoot(ctx context.Context, rootID uuid.UUID) (Result, error) {
	start := time.Now()

	root, err := s.q.GetLibraryRoot(ctx, rootID)
	if err != nil {
		return Result{}, fmt.Errorf("load root: %w", err)
	}
	if err := s.q.MarkScanStarted(ctx, rootID); err != nil {
		return Result{}, fmt.Errorf("mark scan started: %w", err)
	}

	result, scanErr := s.scan(ctx, root)
	result.Duration = time.Since(start)

	finish := dbgen.MarkScanFinishedParams{ID: rootID}
	if scanErr != nil {
		msg := scanErr.Error()
		finish.Error = &msg
	}
	if err := s.q.MarkScanFinished(ctx, finish); err != nil {
		s.log.Error("mark scan finished failed", "error", err)
	}
	return result, scanErr
}

func (s *Scanner) scan(ctx context.Context, root dbgen.LibraryRoot) (Result, error) {
	var result Result

	// Everything the database already knows about this root, keyed by path.
	// One query up front beats a lookup per file: the overwhelmingly common
	// case is a library that has barely changed.
	known, err := s.q.ListTrackStatsForRoot(ctx, root.ID)
	if err != nil {
		return result, fmt.Errorf("load existing tracks: %w", err)
	}
	type stat struct {
		id    uuid.UUID
		size  int64
		mtime time.Time
	}
	existing := make(map[string]stat, len(known))
	for _, k := range known {
		existing[k.RelPath] = stat{id: k.ID, size: k.Size, mtime: k.Mtime}
	}

	// Paths still present on disk. What remains in `existing` afterwards has
	// disappeared.
	seenPaths := make(map[string]bool, len(known))

	type candidate struct {
		relPath string
		absPath string
		size    int64
		mtime   time.Time
	}
	work := make(chan candidate)

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	for range s.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range work {
				outcome, movedFrom, err := s.ingest(ctx, root, c.relPath, c.absPath, c.size, c.mtime)
				mu.Lock()
				switch {
				case err != nil:
					result.Failed++
					s.log.Warn("skipping unreadable file", "path", c.relPath, "error", err)
				case outcome == outcomeAdded:
					result.Added++
				case outcome == outcomeMoved:
					result.Moved++
					// The track's old path is still in `existing` and will not
					// be seen on disk. Without this it would be swept up as
					// vanished and marked missing moments after being moved.
					seenPaths[movedFrom] = true
				default:
					result.Updated++
				}
				mu.Unlock()
			}
		}()
	}

	walkErr := filepath.WalkDir(root.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A single unreadable directory should not abort a 50k-file scan.
			s.log.Warn("skipping unreadable path", "path", path, "error", err)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !IsAudioFile(path) {
			return nil
		}

		rel, err := filepath.Rel(root.Path, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		info, err := d.Info()
		if err != nil {
			return nil
		}
		// Truncated to microseconds because that is all timestamptz keeps.
		// Comparing a nanosecond-precision filesystem mtime against the
		// round-tripped value would never match, and every rescan would
		// re-ingest the entire library.
		mtime := info.ModTime().UTC().Truncate(time.Microsecond)

		mu.Lock()
		result.Seen++
		seenPaths[rel] = true
		prev, isKnown := existing[rel]
		mu.Unlock()

		// The fast path, and the reason a rescan of an unchanged library takes
		// seconds: same size and same mtime means nothing to re-read.
		if isKnown && prev.size == info.Size() && prev.mtime.Equal(mtime) {
			mu.Lock()
			result.Unchanged++
			mu.Unlock()
			return nil
		}

		select {
		case work <- candidate{relPath: rel, absPath: path, size: info.Size(), mtime: mtime}:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})

	close(work)
	wg.Wait()

	if walkErr != nil {
		return result, fmt.Errorf("walk %s: %w", root.Path, walkErr)
	}

	// Anything known but no longer on disk is marked missing rather than
	// deleted, so playlists and history survive an unmounted drive.
	var vanished []uuid.UUID
	for relPath, st := range existing {
		if !seenPaths[relPath] {
			vanished = append(vanished, st.id)
		}
	}
	if len(vanished) > 0 {
		n, err := s.q.MarkTracksMissing(ctx, vanished)
		if err != nil {
			return result, fmt.Errorf("mark missing: %w", err)
		}
		result.Missing = int(n)
	}

	return result, nil
}

type outcome int

const (
	outcomeUpdated outcome = iota
	outcomeAdded
	outcomeMoved
)

// ingest reads one file and writes it, along with its artist, album, genres,
// cover art, and search row, in a single transaction.
// The second return value is the path a moved track came from, so the caller
// can keep the vanished sweep from marking it missing.
func (s *Scanner) ingest(ctx context.Context, root dbgen.LibraryRoot, relPath, absPath string, size int64, mtime time.Time) (outcome, string, error) {
	meta, hash, err := Probe(absPath)
	if err != nil {
		return outcomeUpdated, "", err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return outcomeUpdated, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// A file with an unchanged content hash that the database knows at a
	// different path is the same track, relocated. Updating in place keeps its
	// id, and therefore its playlist entries and play history.
	//
	// A matching hash alone is not enough: a *copy* has the same content while
	// the original is still there, and treating that as a move would relocate
	// the original's row and leave the real file with none. The source file
	// must actually be gone.
	if _, err := q.GetTrackByPath(ctx, dbgen.GetTrackByPathParams{RootID: root.ID, RelPath: relPath}); errors.Is(err, pgx.ErrNoRows) {
		if prior, err := q.FindTrackByHash(ctx, dbgen.FindTrackByHashParams{
			ContentHash: hash, RootID: root.ID,
		}); err == nil && prior.RelPath != relPath && s.sourceIsGone(root, prior) {
			if _, err := q.MoveTrack(ctx, dbgen.MoveTrackParams{
				ID: prior.ID, RelPath: relPath, Size: size, Mtime: mtime,
			}); err != nil {
				return outcomeUpdated, "", fmt.Errorf("move track: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return outcomeUpdated, "", err
			}
			return outcomeMoved, prior.RelPath, nil
		}
	}

	artistID, err := s.upsertArtist(ctx, q, meta.Artist)
	if err != nil {
		return outcomeUpdated, "", err
	}
	// Falling back to the track artist keeps every track of a single-artist
	// album on one album row, which is what "album artist missing" almost
	// always means in practice.
	albumArtistName := meta.AlbumArtist
	if albumArtistName == "" {
		albumArtistName = meta.Artist
	}
	albumArtistID, err := s.upsertArtist(ctx, q, albumArtistName)
	if err != nil {
		return outcomeUpdated, "", err
	}

	coverArtID, err := s.resolveCoverArt(ctx, q, meta, absPath)
	if err != nil {
		// Art is decoration; a broken image must not cost us the track.
		s.log.Warn("cover art failed", "path", relPath, "error", err)
		coverArtID = pgtype.UUID{}
	}

	albumID, err := s.upsertAlbum(ctx, q, meta, albumArtistID, coverArtID)
	if err != nil {
		return outcomeUpdated, "", err
	}

	params := dbgen.UpsertTrackParams{
		RootID:        root.ID,
		RelPath:       relPath,
		Size:          size,
		Mtime:         mtime,
		ContentHash:   hash,
		Suffix:        meta.Suffix,
		Title:         meta.Title,
		AlbumID:       albumID,
		ArtistID:      artistID,
		AlbumArtistID: albumArtistID,
		CoverArtID:    coverArtID,
	}
	if meta.TrackNo > 0 {
		v := int32(meta.TrackNo)
		params.TrackNo = &v
	}
	if meta.DiscNo > 0 {
		v := int32(meta.DiscNo)
		params.DiscNo = &v
	}
	if meta.Year > 0 {
		v := int32(meta.Year)
		params.Year = &v
	}
	if meta.Codec != "" {
		params.Codec = &meta.Codec
	}

	existed := true
	if _, err := q.GetTrackByPath(ctx, dbgen.GetTrackByPathParams{RootID: root.ID, RelPath: relPath}); errors.Is(err, pgx.ErrNoRows) {
		existed = false
	}

	track, err := q.UpsertTrack(ctx, params)
	if err != nil {
		return outcomeUpdated, "", fmt.Errorf("upsert track: %w", err)
	}

	if err := s.replaceGenres(ctx, q, track.ID, meta.Genres); err != nil {
		return outcomeUpdated, "", err
	}

	// Written here rather than by trigger: the effective values span five
	// tables, and the scanner is the one place that already has them all.
	haystack := buildHaystack(meta.Title, meta.Artist, meta.Album, albumArtistName, meta.Genres)
	if err := q.UpsertTrackSearch(ctx, dbgen.UpsertTrackSearchParams{
		TrackID: track.ID, Haystack: haystack,
	}); err != nil {
		return outcomeUpdated, "", fmt.Errorf("upsert search row: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return outcomeUpdated, "", err
	}
	if existed {
		return outcomeUpdated, "", nil
	}
	return outcomeAdded, "", nil
}

// sourceIsGone reports whether a candidate move's source file has actually
// disappeared. Already-missing rows count without touching the disk.
func (s *Scanner) sourceIsGone(root dbgen.LibraryRoot, prior dbgen.Track) bool {
	if prior.MissingAt.Valid {
		return true
	}
	_, err := os.Stat(filepath.Join(root.Path, filepath.FromSlash(prior.RelPath)))
	return errors.Is(err, os.ErrNotExist)
}

func (s *Scanner) upsertArtist(ctx context.Context, q *dbgen.Queries, name string) (pgtype.UUID, error) {
	norm := Normalize(name)
	if norm == "" {
		return pgtype.UUID{}, nil
	}
	artist, err := q.UpsertArtist(ctx, dbgen.UpsertArtistParams{
		Name: name, NormName: norm,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("upsert artist %q: %w", name, err)
	}
	return pgtype.UUID{Bytes: artist.ID, Valid: true}, nil
}

func (s *Scanner) upsertAlbum(ctx context.Context, q *dbgen.Queries, meta Metadata, albumArtistID, coverArtID pgtype.UUID) (pgtype.UUID, error) {
	norm := Normalize(meta.Album)
	if norm == "" {
		return pgtype.UUID{}, nil
	}
	params := dbgen.UpsertAlbumParams{
		Name:          meta.Album,
		NormName:      norm,
		AlbumArtistID: albumArtistID,
		CoverArtID:    coverArtID,
	}
	if meta.Year > 0 {
		v := int32(meta.Year)
		params.Year = &v
	}
	album, err := q.UpsertAlbum(ctx, params)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("upsert album %q: %w", meta.Album, err)
	}
	return pgtype.UUID{Bytes: album.ID, Valid: true}, nil
}

func (s *Scanner) replaceGenres(ctx context.Context, q *dbgen.Queries, trackID uuid.UUID, names []string) error {
	ids := make([]uuid.UUID, 0, len(names))
	for _, name := range names {
		genre, err := q.UpsertGenre(ctx, name)
		if err != nil {
			return fmt.Errorf("upsert genre %q: %w", name, err)
		}
		ids = append(ids, genre.ID)
	}
	if err := q.ReplaceTrackGenres(ctx, dbgen.ReplaceTrackGenresParams{
		TrackID: trackID, GenreIds: ids,
	}); err != nil {
		return fmt.Errorf("replace genres: %w", err)
	}
	return nil
}

// resolveCoverArt stores embedded or sidecar art, addressed by content hash so
// an album's shared artwork is stored once rather than once per track.
func (s *Scanner) resolveCoverArt(ctx context.Context, q *dbgen.Queries, meta Metadata, absPath string) (pgtype.UUID, error) {
	data, mime := meta.Art, meta.ArtMIME
	source := "embedded"

	if len(data) == 0 {
		sidecar, ok := FindSidecarArt(absPath)
		if !ok {
			return pgtype.UUID{}, nil
		}
		raw, err := os.ReadFile(sidecar)
		if err != nil {
			return pgtype.UUID{}, nil // not worth failing the track over
		}
		data, source = raw, "sidecar"
		mime = http.DetectContentType(raw)
	}
	if len(data) == 0 {
		return pgtype.UUID{}, nil
	}
	if mime == "" {
		mime = http.DetectContentType(data)
	}

	sum := sha256.Sum256(data)
	key := "art/" + hex.EncodeToString(sum[:2]) + "/" + hex.EncodeToString(sum[:]) + extForMIME(mime)

	// Content-addressed, so an existing blob is already the right bytes.
	if _, err := s.blobs.Stat(ctx, key); err != nil {
		if !errors.Is(err, blobstore.ErrNotFound) {
			return pgtype.UUID{}, err
		}
		w, err := s.blobs.Create(ctx, key)
		if err != nil {
			return pgtype.UUID{}, err
		}
		if _, err := w.Write(data); err != nil {
			w.Close()
			return pgtype.UUID{}, err
		}
		if err := w.Commit(); err != nil {
			w.Close()
			return pgtype.UUID{}, err
		}
		w.Close()
	}

	params := dbgen.UpsertCoverArtParams{
		Hash:    sum[:],
		BlobKey: key,
		Source:  source,
		Mime:    mime,
		Bytes:   int64(len(data)),
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		w, h := int32(cfg.Width), int32(cfg.Height)
		params.Width, params.Height = &w, &h
	}

	art, err := q.UpsertCoverArt(ctx, params)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("upsert cover art: %w", err)
	}
	return pgtype.UUID{Bytes: art.ID, Valid: true}, nil
}

func extForMIME(mime string) string {
	switch {
	case bytes.Contains([]byte(mime), []byte("png")):
		return ".png"
	case bytes.Contains([]byte(mime), []byte("webp")):
		return ".webp"
	case bytes.Contains([]byte(mime), []byte("gif")):
		return ".gif"
	default:
		return ".jpg"
	}
}

// buildHaystack assembles the text the search index folds and tokenises.
func buildHaystack(title, artist, album, albumArtist string, genres []string) string {
	var b bytes.Buffer
	for _, part := range append([]string{title, artist, album, albumArtist}, genres...) {
		if part == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(part)
	}
	return b.String()
}

// shouldSkipDir prunes directories that never contain library audio. Skipping
// them keeps a scan from wandering into version-control objects or the
// thousands of files a macOS volume hides at its root.
func shouldSkipDir(name string) bool {
	switch name {
	case ".git", ".svn", "node_modules", "@eaDir", ".Trash", ".Trashes",
		"#recycle", "$RECYCLE.BIN", "System Volume Information", ".stfolder":
		return true
	}
	// Hidden directories, but not the root itself when it is given as ".".
	return len(name) > 1 && name[0] == '.'
}
