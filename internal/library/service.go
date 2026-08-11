package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jonathanng/echo/internal/blobstore"
	"github.com/jonathanng/echo/internal/db/dbgen"
	"github.com/jonathanng/echo/internal/jobs"
)

// Service ties the scanner, the watcher, and the job queue together.
type Service struct {
	pool    *pgxpool.Pool
	q       *dbgen.Queries
	scanner *Scanner
	queue   *jobs.Queue
	blobs   blobstore.Store
	log     *slog.Logger
}

func NewService(pool *pgxpool.Pool, queue *jobs.Queue, blobs blobstore.Store, log *slog.Logger) *Service {
	return &Service{
		pool:    pool,
		q:       dbgen.New(pool),
		scanner: NewScanner(pool, blobs, log),
		queue:   queue,
		blobs:   blobs,
		log:     log,
	}
}

// Job payloads.
type (
	ScanRootPayload struct {
		RootID uuid.UUID `json:"rootId"`
	}
	ScanFilePayload struct {
		RootID  uuid.UUID `json:"rootId"`
		AbsPath string    `json:"absPath"`
	}
)

// RegisterHandlers wires the library's job types into the queue.
func (s *Service) RegisterHandlers() {
	s.queue.Register(jobs.TypeScanRoot, s.handleScanRoot)
	s.queue.Register(jobs.TypeScanFile, s.handleScanFile)
}

// SyncRoots registers configured paths and returns them.
func (s *Service) SyncRoots(ctx context.Context, paths []string, writablePath string) ([]dbgen.LibraryRoot, error) {
	return s.scanner.SyncRoots(ctx, paths, writablePath)
}

// EnqueueScanAll queues a scan of every enabled root.
func (s *Service) EnqueueScanAll(ctx context.Context) (int, error) {
	roots, err := s.q.ListLibraryRoots(ctx)
	if err != nil {
		return 0, err
	}
	for _, root := range roots {
		if _, err := s.queue.Enqueue(ctx, jobs.TypeScanRoot,
			ScanRootPayload{RootID: root.ID},
			// Dedupe per root: asking twice while one is queued should not run
			// the same walk twice.
			jobs.EnqueueOpts{DedupeKey: "scan_root:" + root.ID.String(), Priority: 10},
		); err != nil {
			return 0, err
		}
	}
	return len(roots), nil
}

// EnqueueFile queues a single file, used by the watcher.
func (s *Service) EnqueueFile(rootID uuid.UUID, absPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := s.queue.Enqueue(ctx, jobs.TypeScanFile,
		ScanFilePayload{RootID: rootID, AbsPath: absPath},
		jobs.EnqueueOpts{DedupeKey: "scan_file:" + absPath, Priority: 20},
	); err != nil {
		s.log.Error("enqueue file scan failed", "path", absPath, "error", err)
	}
}

func (s *Service) handleScanRoot(ctx context.Context, job dbgen.Job) error {
	var payload ScanRootPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}

	s.log.Info("scan started", "root", payload.RootID)
	result, err := s.scanner.ScanRoot(ctx, payload.RootID)
	if err != nil {
		return err
	}
	s.log.Info("scan finished", append([]any{"root", payload.RootID}, result.LogArgs()...)...)

	// Reconciliation leaves artists and albums behind when their last track
	// disappears; sweeping here keeps browse facets from filling with entries
	// that match nothing.
	s.sweepOrphans(ctx)
	return nil
}

// sweepOrphans removes albums and then artists that nothing references. The
// order matters: an artist can only be judged orphaned once the albums that
// referenced it are gone.
func (s *Service) sweepOrphans(ctx context.Context) {
	if _, err := s.q.DeleteOrphanedAlbums(ctx); err != nil {
		s.log.Warn("orphaned album sweep failed", "error", err)
		return
	}
	if _, err := s.q.DeleteOrphanedArtists(ctx); err != nil {
		s.log.Warn("orphaned artist sweep failed", "error", err)
	}
}

// handleScanFile re-reads one file, for the watcher's incremental path.
func (s *Service) handleScanFile(ctx context.Context, job dbgen.Job) error {
	var payload ScanFilePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}

	root, err := s.q.GetLibraryRoot(ctx, payload.RootID)
	if err != nil {
		return fmt.Errorf("load root: %w", err)
	}

	rel, err := filepath.Rel(root.Path, payload.AbsPath)
	if err != nil {
		return fmt.Errorf("path %q is outside root %q", payload.AbsPath, root.Path)
	}
	rel = filepath.ToSlash(rel)

	info, err := os.Stat(payload.AbsPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// Deleted between the event and now. Mark it missing rather than
		// delete: it may be a move whose other half has not arrived yet.
		track, err := s.q.GetTrackByPath(ctx, dbgen.GetTrackByPathParams{
			RootID: root.ID, RelPath: rel,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if _, err := s.q.MarkTracksMissing(ctx, []uuid.UUID{track.ID}); err != nil {
			return err
		}
		s.log.Info("track went missing", "path", rel)
		return nil
	}

	// Truncated to microseconds to match what timestamptz stores, so a later
	// full scan recognises this file as unchanged.
	mtime := info.ModTime().UTC().Truncate(time.Microsecond)
	outcome, _, err := s.scanner.ingest(ctx, root, rel, payload.AbsPath, info.Size(), mtime)
	if err != nil {
		return err
	}
	s.log.Debug("file scanned", "path", rel, "outcome", outcome)
	return nil
}

// StartWatcher launches the filesystem watcher for every enabled root.
//
// Returns nil when watching is unavailable — an NFS mount that does not deliver
// events, or a host at its inotify limit — because a library that only updates
// on rescan is degraded, not broken.
func (s *Service) StartWatcher(ctx context.Context) error {
	roots, err := s.q.ListLibraryRoots(ctx)
	if err != nil {
		return err
	}

	watcher, err := NewWatcher(s.log, s.EnqueueFile)
	if err != nil {
		s.log.Warn("filesystem watching unavailable; changes will only be picked "+
			"up by a rescan", "error", err)
		return nil
	}

	for _, root := range roots {
		if err := watcher.Add(root.ID, root.Path); err != nil {
			s.log.Warn("could not watch root", "path", root.Path, "error", err)
		}
	}
	go watcher.Run(ctx)
	return nil
}

// Stats reports library totals for the admin surface.
func (s *Service) Stats(ctx context.Context) (dbgen.LibraryStatsRow, error) {
	return s.q.LibraryStats(ctx)
}

// PurgeMissing permanently removes tracks that have been missing longer than
// the grace period, along with any cover art and reconciliation rows that are
// left unreferenced.
func (s *Service) PurgeMissing(ctx context.Context, olderThan time.Duration) (int64, error) {
	n, err := s.q.DeleteMissingTracksOlderThan(ctx,
		pgtype.Interval{Microseconds: olderThan.Microseconds(), Valid: true})
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}

	orphanKeys, err := s.q.DeleteOrphanedCoverArt(ctx)
	if err != nil {
		s.log.Warn("orphaned cover art sweep failed", "error", err)
	}
	for _, key := range orphanKeys {
		if err := s.blobs.Delete(ctx, key); err != nil {
			s.log.Warn("delete orphaned art blob failed", "key", key, "error", err)
		}
	}
	s.sweepOrphans(ctx)
	return n, nil
}
