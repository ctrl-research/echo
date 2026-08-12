package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jonathanng/echo/internal/blobstore"
	"github.com/jonathanng/echo/internal/db/dbgen"
	"github.com/jonathanng/echo/internal/jobs"
)

// Job types owned by this package.
const (
	TypeDownload = "yt_download"
	TypeEvict    = "yt_cache_evict"
)

var (
	// ErrNotCached means the item exists but holds no bytes right now.
	ErrNotCached = errors.New("youtube: item is not cached")
	// ErrNotFound means the video is unknown to this instance.
	ErrNotFound = errors.New("youtube: unknown video")
)

// Options tune the cache.
type Options struct {
	// TTL is the sliding window from last access.
	TTL time.Duration
	// MaxLifetime caps how far the sliding window can push expiry from the
	// original download, so one much-played item cannot hold space forever.
	MaxLifetime time.Duration
	// MaxBytes triggers LRU eviction once the cache exceeds it.
	MaxBytes int64
	// PromoteDir is the writable library root promoted downloads are copied to.
	PromoteDir string
}

type Service struct {
	q     *dbgen.Queries
	pool  *pgxpool.Pool
	blobs blobstore.Store
	dl    Downloader
	queue *jobs.Queue
	log   *slog.Logger
	opts  Options
}

func NewService(pool *pgxpool.Pool, blobs blobstore.Store, dl Downloader, queue *jobs.Queue, log *slog.Logger, opts Options) *Service {
	if opts.TTL <= 0 {
		opts.TTL = 48 * time.Hour
	}
	if opts.MaxLifetime <= 0 {
		opts.MaxLifetime = 14 * 24 * time.Hour
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 5 << 30
	}
	return &Service{
		q: dbgen.New(pool), pool: pool, blobs: blobs, dl: dl,
		queue: queue, log: log, opts: opts,
	}
}

func (s *Service) Available() bool { return s.dl.Available() }

// Version reports the yt-dlp build in use. Surfaced through the API because
// extraction breaks every few weeks and "which version is running" is the first
// question when it does.
func (s *Service) Version(ctx context.Context) string { return s.dl.Version(ctx) }

func (s *Service) RegisterHandlers() {
	s.queue.Register(TypeDownload, s.handleDownload)
	s.queue.Register(TypeEvict, s.handleEvict)
}

// CacheKey is where a video's audio lives in the blob store.
func CacheKey(videoID string) string { return "youtube/" + videoID + ".opus" }

// ---- search ---------------------------------------------------------------------

func (s *Service) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	return s.dl.Search(ctx, query, limit)
}

// ---- preparation ----------------------------------------------------------------

type DownloadPayload struct {
	VideoID string `json:"videoId"`
}

// Prepare records the video and queues a download if it is not already cached.
//
// Returns the current row so the caller can poll. Downloading rather than
// proxying the CDN URL is what makes the result seekable: a proxied stream has
// an unknown length and cannot be rewound, and the URLs are IP-bound and expire
// within hours.
func (s *Service) Prepare(ctx context.Context, meta SearchResult) (dbgen.YtItem, error) {
	params := dbgen.UpsertYTItemParams{
		VideoID: meta.VideoID, Title: meta.Title, Uploader: meta.Uploader,
	}
	if meta.DurationMs > 0 {
		v := int32(meta.DurationMs)
		params.DurationMs = &v
	}
	if meta.ThumbnailURL != "" {
		params.ThumbnailUrl = &meta.ThumbnailURL
	}

	item, err := s.q.UpsertYTItem(ctx, params)
	if err != nil {
		return dbgen.YtItem{}, fmt.Errorf("record item: %w", err)
	}

	// Already have the bytes: just push the expiry out.
	if item.State == dbgen.YtStateReady && item.BlobKey != nil {
		if err := s.Touch(ctx, item.VideoID); err != nil {
			s.log.Warn("touch failed", "video", item.VideoID, "error", err)
		}
		return item, nil
	}
	if item.State == dbgen.YtStateDownloading {
		return item, nil
	}

	if _, err := s.queue.Enqueue(ctx, TypeDownload,
		DownloadPayload{VideoID: meta.VideoID},
		// Deduped per video so pressing play twice does not download twice.
		jobs.EnqueueOpts{DedupeKey: "yt_download:" + meta.VideoID, Priority: 50},
	); err != nil {
		return item, fmt.Errorf("queue download: %w", err)
	}
	return item, nil
}

func (s *Service) handleDownload(ctx context.Context, job dbgen.Job) error {
	var payload DownloadPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}

	if err := s.q.MarkYTDownloading(ctx, payload.VideoID); err != nil {
		return fmt.Errorf("mark downloading: %w", err)
	}

	result, err := s.dl.Download(ctx, payload.VideoID)
	if err != nil {
		// Recorded rather than only returned, so the client polling the item
		// can show why it failed instead of spinning forever.
		if markErr := s.q.MarkYTFailed(ctx, dbgen.MarkYTFailedParams{
			VideoID: payload.VideoID, Error: truncate(err.Error(), 1000),
		}); markErr != nil {
			s.log.Error("mark failed", "video", payload.VideoID, "error", markErr)
		}
		return err
	}
	defer os.Remove(result.Path)

	key := CacheKey(payload.VideoID)
	if err := s.storeFile(ctx, key, result.Path); err != nil {
		return fmt.Errorf("store audio: %w", err)
	}

	ready := dbgen.MarkYTReadyParams{
		VideoID: payload.VideoID, BlobKey: key, Bytes: result.Bytes,
		Title: result.Title, Uploader: result.Uploader,
		Ttl: interval(s.opts.TTL),
	}
	if result.DurationMs > 0 {
		v := int32(result.DurationMs)
		ready.DurationMs = &v
	}
	if _, err := s.q.MarkYTReady(ctx, ready); err != nil {
		return fmt.Errorf("mark ready: %w", err)
	}

	s.log.Info("youtube item cached", "video", payload.VideoID,
		"title", result.Title, "bytes", result.Bytes)

	// A new arrival may push the cache over its ceiling.
	if _, err := s.queue.Enqueue(ctx, TypeEvict, nil,
		jobs.EnqueueOpts{DedupeKey: "yt_cache_evict", Priority: 1}); err != nil {
		s.log.Warn("queue eviction failed", "error", err)
	}
	return nil
}

func (s *Service) storeFile(ctx context.Context, key, path string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	w, err := s.blobs.Create(ctx, key)
	if err != nil {
		return err
	}
	defer w.Close()

	if _, err := io.Copy(w, src); err != nil {
		return err
	}
	return w.Commit()
}

// ---- access ----------------------------------------------------------------------

// Get returns an item, or ErrNotFound.
func (s *Service) Get(ctx context.Context, videoID string) (dbgen.YtItem, error) {
	item, err := s.q.GetYTItem(ctx, videoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return dbgen.YtItem{}, ErrNotFound
		}
		return dbgen.YtItem{}, err
	}
	return item, nil
}

// Touch slides the expiry forward, bounded by MaxLifetime.
func (s *Service) Touch(ctx context.Context, videoID string) error {
	return s.q.TouchYTItem(ctx, dbgen.TouchYTItemParams{
		VideoID:     videoID,
		Ttl:         interval(s.opts.TTL),
		MaxLifetime: interval(s.opts.MaxLifetime),
	})
}

// BlobKey returns where a ready item's bytes live, touching it as a side effect
// because reading them is exactly what "recently used" means.
func (s *Service) BlobKey(ctx context.Context, videoID string) (string, error) {
	item, err := s.Get(ctx, videoID)
	if err != nil {
		return "", err
	}
	if item.State != dbgen.YtStateReady || item.BlobKey == nil {
		return "", ErrNotCached
	}
	if err := s.Touch(ctx, videoID); err != nil {
		s.log.Warn("touch failed", "video", videoID, "error", err)
	}
	return *item.BlobKey, nil
}

// ---- eviction ----------------------------------------------------------------------

// Evict removes expired items, then trims by least-recently-used until the
// cache fits. Promoted items are never touched: their bytes live under a
// library root now.
func (s *Service) Evict(ctx context.Context) (int, error) {
	removed := 0

	expired, err := s.q.ExpiredYTItems(ctx)
	if err != nil {
		return 0, fmt.Errorf("list expired: %w", err)
	}
	for _, item := range expired {
		if err := s.evictOne(ctx, item); err != nil {
			s.log.Warn("evict failed", "video", item.VideoID, "error", err)
			continue
		}
		removed++
	}

	total, err := s.q.YTCacheBytes(ctx)
	if err != nil {
		return removed, fmt.Errorf("cache size: %w", err)
	}
	if total <= s.opts.MaxBytes {
		return removed, nil
	}

	// Over the ceiling: drop least-recently-used until it fits.
	candidates, err := s.q.YTItemsByLRU(ctx, 500)
	if err != nil {
		return removed, fmt.Errorf("list lru: %w", err)
	}
	for _, item := range candidates {
		if total <= s.opts.MaxBytes {
			break
		}
		size := int64(0)
		if item.Bytes != nil {
			size = *item.Bytes
		}
		if err := s.evictOne(ctx, item); err != nil {
			s.log.Warn("evict failed", "video", item.VideoID, "error", err)
			continue
		}
		total -= size
		removed++
	}
	return removed, nil
}

func (s *Service) evictOne(ctx context.Context, item dbgen.YtItem) error {
	if item.BlobKey != nil {
		if err := s.blobs.Delete(ctx, *item.BlobKey); err != nil {
			return err
		}
	}
	return s.q.MarkYTEvicted(ctx, item.VideoID)
}

func (s *Service) handleEvict(ctx context.Context, _ dbgen.Job) error {
	n, err := s.Evict(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		s.log.Info("youtube cache evicted", "items", n)
	}
	return nil
}

// EvictLoop runs the sweep periodically until ctx is cancelled. Expiry is a
// time-based event with nothing to trigger it, so something has to look.
func (s *Service) EvictLoop(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.queue.Enqueue(ctx, TypeEvict, nil,
				jobs.EnqueueOpts{DedupeKey: "yt_cache_evict", Priority: 1}); err != nil {
				s.log.Warn("queue eviction failed", "error", err)
			}
		}
	}
}

// ---- promotion -----------------------------------------------------------------------

// Promote copies a cached item into the library, where it becomes an ordinary
// track and stops being subject to eviction.
//
// Copied rather than moved: the cache entry stays valid until the scanner has
// picked the file up, so playback continues uninterrupted in the meantime.
func (s *Service) Promote(ctx context.Context, videoID string) (string, error) {
	if s.opts.PromoteDir == "" {
		return "", errors.New("youtube: no writable library root is configured")
	}

	item, err := s.Get(ctx, videoID)
	if err != nil {
		return "", err
	}
	if item.State != dbgen.YtStateReady || item.BlobKey == nil {
		return "", ErrNotCached
	}
	if item.PromotedTrackID.Valid {
		return "", nil // already promoted; nothing to do
	}

	dir := filepath.Join(s.opts.PromoteDir, sanitise(item.Uploader))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create promote directory: %w", err)
	}
	dest := filepath.Join(dir, sanitise(item.Title)+".opus")

	reader, _, err := s.blobs.Open(ctx, *item.BlobKey)
	if err != nil {
		return "", fmt.Errorf("open cached audio: %w", err)
	}
	defer reader.Close()

	// Written to a temp file and renamed, so the scanner's watcher never sees a
	// half-written file and tries to read tags from it.
	tmp, err := os.CreateTemp(dir, ".promote-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, reader); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		os.Remove(tmpPath)
		return "", err
	}

	s.log.Info("youtube item promoted to library", "video", videoID, "path", dest)
	return dest, nil
}

// LinkPromoted records which track a promoted item became, which exempts it
// from eviction.
func (s *Service) LinkPromoted(ctx context.Context, videoID string, trackID uuid.UUID) error {
	return s.q.MarkYTPromoted(ctx, dbgen.MarkYTPromotedParams{
		VideoID: videoID, PromotedTrackID: pgtype.UUID{Bytes: trackID, Valid: true},
	})
}

// ---- helpers ---------------------------------------------------------------------------

func interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// sanitise makes a title safe as a path component. Video titles routinely
// contain slashes, colons, and control characters.
func sanitise(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20, r == 0x7f:
			// control characters are dropped
		case strings.ContainsRune(`/\:*?"<>|`, r):
			b.WriteRune('-')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	// Only a name made entirely of dots is a problem — "." and ".." resolve to
	// directories. Stripping every leading dot would mangle real titles like
	// "...Baby One More Time".
	if strings.Trim(out, ".") == "" {
		return "Unknown"
	}
	if len(out) > 120 {
		out = strings.TrimSpace(out[:120])
	}
	return out
}
