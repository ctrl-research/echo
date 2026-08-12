//go:build integration

package youtube_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jonathanng/echo/internal/blobstore"
	"github.com/jonathanng/echo/internal/db/dbgen"
	"github.com/jonathanng/echo/internal/dbtest"
	"github.com/jonathanng/echo/internal/jobs"
	"github.com/jonathanng/echo/internal/youtube"
)

func TestMain(m *testing.M) { os.Exit(dbtest.Main(m)) }

// fakeDownloader stands in for yt-dlp.
//
// The real one depends on a binary YouTube breaks every few weeks and on
// network access CI must not have. Everything worth testing — the state
// machine, the sliding TTL, LRU eviction, promotion — is on this side of that
// boundary, which is why the boundary exists.
type fakeDownloader struct {
	available bool
	results   []youtube.SearchResult
	audio     []byte
	failWith  error
	calls     atomic.Int64
	// block, when non-nil, holds Download until it is closed. Used to observe
	// the downloading state.
	block chan struct{}
}

func (f *fakeDownloader) Available() bool                { return f.available }
func (f *fakeDownloader) Version(context.Context) string { return "fake" }

func (f *fakeDownloader) Search(_ context.Context, _ string, _ int) ([]youtube.SearchResult, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	return f.results, nil
}

func (f *fakeDownloader) Download(_ context.Context, videoID string) (youtube.DownloadResult, error) {
	f.calls.Add(1)
	if f.block != nil {
		<-f.block
	}
	if f.failWith != nil {
		return youtube.DownloadResult{}, f.failWith
	}

	tmp, err := os.CreateTemp("", "fake-yt-*.opus")
	if err != nil {
		return youtube.DownloadResult{}, err
	}
	audio := f.audio
	if audio == nil {
		audio = []byte("fake opus bytes for " + videoID)
	}
	if _, err := tmp.Write(audio); err != nil {
		return youtube.DownloadResult{}, err
	}
	tmp.Close()

	return youtube.DownloadResult{
		Path: tmp.Name(), Title: "Fake " + videoID, Uploader: "Fake Channel",
		DurationMs: 210_000, Bytes: int64(len(audio)),
	}, nil
}

type harness struct {
	t     *testing.T
	pool  *pgxpool.Pool
	q     *dbgen.Queries
	blobs blobstore.Store
	dl    *fakeDownloader
	svc   *youtube.Service
	queue *jobs.Queue
	dir   string
}

func newHarness(t *testing.T, opts youtube.Options) *harness {
	t.Helper()
	pool := dbtest.New(t)
	blobs, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blobstore: %v", err)
	}
	promoteDir := t.TempDir()
	if opts.PromoteDir == "" {
		opts.PromoteDir = promoteDir
	}

	dl := &fakeDownloader{available: true}
	queue := jobs.New(pool, dbtest.DiscardLogger())
	svc := youtube.NewService(pool, blobs, dl, queue, dbtest.DiscardLogger(), opts)
	svc.RegisterHandlers()

	return &harness{t: t, pool: pool, q: dbgen.New(pool), blobs: blobs,
		dl: dl, svc: svc, queue: queue, dir: opts.PromoteDir}
}

// runWorkers starts the queue for the duration of the test.
func (h *harness) runWorkers() {
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	go h.queue.Run(ctx, 2)
}

func (h *harness) waitForState(videoID string, want dbgen.YtState, timeout time.Duration) dbgen.YtItem {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		item, err := h.svc.Get(context.Background(), videoID)
		if err == nil && item.State == want {
			return item
		}
		time.Sleep(25 * time.Millisecond)
	}
	item, _ := h.svc.Get(context.Background(), videoID)
	h.t.Fatalf("timed out waiting for %s to reach %q; it is %q (error: %v)",
		videoID, want, item.State, item.Error)
	return dbgen.YtItem{}
}

// ---- download lifecycle ------------------------------------------------------------

func TestPrepareDownloadsAndCaches(t *testing.T) {
	h := newHarness(t, youtube.Options{})
	h.runWorkers()

	if _, err := h.svc.Prepare(context.Background(), youtube.SearchResult{
		VideoID: "abc123", Title: "A Song", Uploader: "Someone", DurationMs: 200_000,
	}); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	item := h.waitForState("abc123", dbgen.YtStateReady, 15*time.Second)
	if item.BlobKey == nil {
		t.Fatal("ready item has no blob key")
	}
	if item.Bytes == nil || *item.Bytes == 0 {
		t.Error("ready item records no size")
	}
	if !item.ExpiresAt.Valid {
		t.Error("ready item has no expiry")
	}

	// The bytes are really there.
	if _, err := h.blobs.Stat(context.Background(), *item.BlobKey); err != nil {
		t.Errorf("cached blob missing: %v", err)
	}

	key, err := h.svc.BlobKey(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("BlobKey: %v", err)
	}
	if key != youtube.CacheKey("abc123") {
		t.Errorf("key = %q, want %q", key, youtube.CacheKey("abc123"))
	}
}

// Pressing play twice must not download twice.
func TestPrepareIsDeduplicated(t *testing.T) {
	h := newHarness(t, youtube.Options{})
	ctx := context.Background()

	for range 5 {
		if _, err := h.svc.Prepare(ctx, youtube.SearchResult{VideoID: "dedupe1"}); err != nil {
			t.Fatalf("prepare: %v", err)
		}
	}

	var queued int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE type = 'yt_download'`).Scan(&queued); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if queued != 1 {
		t.Errorf("%d download jobs queued, want 1", queued)
	}
}

// An already-cached item plays immediately and is not re-downloaded.
func TestPrepareSkipsDownloadWhenCached(t *testing.T) {
	h := newHarness(t, youtube.Options{})
	h.runWorkers()
	ctx := context.Background()

	h.svc.Prepare(ctx, youtube.SearchResult{VideoID: "cached1"})
	h.waitForState("cached1", dbgen.YtStateReady, 15*time.Second)
	before := h.dl.calls.Load()

	if _, err := h.svc.Prepare(ctx, youtube.SearchResult{VideoID: "cached1"}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	if got := h.dl.calls.Load(); got != before {
		t.Errorf("downloaded again: %d calls, want %d", got, before)
	}
}

// A failure is recorded on the item, so a client polling it can say why rather
// than spinning forever.
func TestFailedDownloadIsRecorded(t *testing.T) {
	h := newHarness(t, youtube.Options{})
	h.dl.failWith = errors.New("video unavailable")
	h.runWorkers()

	h.svc.Prepare(context.Background(), youtube.SearchResult{VideoID: "bad1"})
	item := h.waitForState("bad1", dbgen.YtStateFailed, 30*time.Second)

	if item.Error == nil || *item.Error == "" {
		t.Error("no failure reason recorded")
	}
}

func TestStreamingBeforeReadyIsNotCached(t *testing.T) {
	h := newHarness(t, youtube.Options{})
	ctx := context.Background()

	h.svc.Prepare(ctx, youtube.SearchResult{VideoID: "pending1"})
	if _, err := h.svc.BlobKey(ctx, "pending1"); !errors.Is(err, youtube.ErrNotCached) {
		t.Errorf("err = %v, want ErrNotCached", err)
	}
}

func TestUnknownVideoIsNotFound(t *testing.T) {
	h := newHarness(t, youtube.Options{})
	if _, err := h.svc.Get(context.Background(), "nope"); !errors.Is(err, youtube.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// ---- sliding TTL ----------------------------------------------------------------------

// The design's headline cache property: playing something pushes its expiry
// out, so a track you listen to daily does not vanish mid-week.
func TestPlayingSlidesTheExpiry(t *testing.T) {
	h := newHarness(t, youtube.Options{TTL: 48 * time.Hour})
	h.runWorkers()
	ctx := context.Background()

	h.svc.Prepare(ctx, youtube.SearchResult{VideoID: "slide1"})
	h.waitForState("slide1", dbgen.YtStateReady, 15*time.Second)

	// Backdate so the expiry is nearly due.
	if _, err := h.pool.Exec(ctx,
		`UPDATE yt_items SET expires_at = now() + interval '1 hour' WHERE video_id = 'slide1'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if _, err := h.svc.BlobKey(ctx, "slide1"); err != nil {
		t.Fatalf("BlobKey: %v", err)
	}

	item, _ := h.svc.Get(ctx, "slide1")
	if time.Until(item.ExpiresAt.Time) < 40*time.Hour {
		t.Errorf("expiry is %v away, want the full TTL back", time.Until(item.ExpiresAt.Time))
	}
}

// The sliding window is bounded, so one much-played item cannot hold cache
// space indefinitely.
func TestSlidingExpiryIsCappedByMaxLifetime(t *testing.T) {
	h := newHarness(t, youtube.Options{TTL: 48 * time.Hour, MaxLifetime: 72 * time.Hour})
	h.runWorkers()
	ctx := context.Background()

	h.svc.Prepare(ctx, youtube.SearchResult{VideoID: "capped1"})
	h.waitForState("capped1", dbgen.YtStateReady, 15*time.Second)

	// Model an item downloaded two days ago and played constantly since.
	if _, err := h.pool.Exec(ctx,
		`UPDATE yt_items SET cached_at = now() - interval '48 hours' WHERE video_id = 'capped1'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := h.svc.Touch(ctx, "capped1"); err != nil {
		t.Fatalf("touch: %v", err)
	}

	item, _ := h.svc.Get(ctx, "capped1")
	// cached_at + 72h is 24h away, which is nearer than now + 48h.
	if until := time.Until(item.ExpiresAt.Time); until > 25*time.Hour {
		t.Errorf("expiry is %v away; the max lifetime cap did not apply", until)
	}
}

// ---- eviction ---------------------------------------------------------------------------

func TestExpiredItemsAreEvicted(t *testing.T) {
	h := newHarness(t, youtube.Options{})
	h.runWorkers()
	ctx := context.Background()

	h.svc.Prepare(ctx, youtube.SearchResult{VideoID: "expire1"})
	item := h.waitForState("expire1", dbgen.YtStateReady, 15*time.Second)
	key := *item.BlobKey

	if _, err := h.pool.Exec(ctx,
		`UPDATE yt_items SET expires_at = now() - interval '1 minute' WHERE video_id = 'expire1'`); err != nil {
		t.Fatalf("expire: %v", err)
	}

	n, err := h.svc.Evict(ctx)
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if n != 1 {
		t.Errorf("evicted %d items, want 1", n)
	}

	after, _ := h.svc.Get(ctx, "expire1")
	if after.State != dbgen.YtStateEvicted {
		t.Errorf("state = %q, want evicted", after.State)
	}
	if after.BlobKey != nil {
		t.Error("evicted item still records a blob key")
	}
	// And the bytes are actually gone, not merely unreferenced.
	if _, err := h.blobs.Stat(ctx, key); !errors.Is(err, blobstore.ErrNotFound) {
		t.Errorf("blob still present after eviction: %v", err)
	}
}

// A live item must survive the sweep.
func TestUnexpiredItemsSurviveEviction(t *testing.T) {
	h := newHarness(t, youtube.Options{})
	h.runWorkers()
	ctx := context.Background()

	h.svc.Prepare(ctx, youtube.SearchResult{VideoID: "keep1"})
	h.waitForState("keep1", dbgen.YtStateReady, 15*time.Second)

	if _, err := h.svc.Evict(ctx); err != nil {
		t.Fatalf("evict: %v", err)
	}
	item, _ := h.svc.Get(ctx, "keep1")
	if item.State != dbgen.YtStateReady {
		t.Errorf("state = %q, want ready", item.State)
	}
}

// Over the size ceiling, the least recently used goes first.
func TestOversizedCacheEvictsLeastRecentlyUsed(t *testing.T) {
	h := newHarness(t, youtube.Options{MaxBytes: 100})
	h.runWorkers()
	ctx := context.Background()

	// Exactly 40 bytes each, so three items (120) exceed the 100 byte ceiling
	// and the sweep has to drop at least one.
	h.dl.audio = make([]byte, 40)
	for _, id := range []string{"lru1", "lru2", "lru3"} {
		h.svc.Prepare(ctx, youtube.SearchResult{VideoID: id})
		h.waitForState(id, dbgen.YtStateReady, 15*time.Second)
	}

	// Make lru1 the oldest by a clear margin.
	if _, err := h.pool.Exec(ctx, `
		UPDATE yt_items SET last_accessed_at = now() - interval '10 days' WHERE video_id = 'lru1';
		UPDATE yt_items SET last_accessed_at = now() - interval '1 minute' WHERE video_id <> 'lru1';
	`); err != nil {
		t.Fatalf("age items: %v", err)
	}

	if _, err := h.svc.Evict(ctx); err != nil {
		t.Fatalf("evict: %v", err)
	}

	oldest, _ := h.svc.Get(ctx, "lru1")
	if oldest.State != dbgen.YtStateEvicted {
		t.Errorf("least recently used state = %q, want evicted", oldest.State)
	}

	total, err := h.q.YTCacheBytes(ctx)
	if err != nil {
		t.Fatalf("cache size: %v", err)
	}
	if total > 100 {
		t.Errorf("cache is %d bytes, over the 100 byte ceiling", total)
	}
}

// ---- promotion ---------------------------------------------------------------------------

func TestPromoteCopiesIntoTheLibrary(t *testing.T) {
	h := newHarness(t, youtube.Options{})
	h.runWorkers()
	ctx := context.Background()

	h.svc.Prepare(ctx, youtube.SearchResult{VideoID: "promote1"})
	h.waitForState("promote1", dbgen.YtStateReady, 15*time.Second)

	path, err := h.svc.Promote(ctx, "promote1")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if path == "" {
		t.Fatal("promote returned no path")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("promoted file missing: %v", err)
	}
	// Filed under the uploader, which is how the library organises it.
	if filepath.Base(filepath.Dir(path)) != "Fake Channel" {
		t.Errorf("promoted into %q, want a directory named for the uploader", filepath.Dir(path))
	}

	// The cache entry stays valid until the scanner picks the file up, so
	// playback is uninterrupted meanwhile.
	if _, err := h.svc.BlobKey(ctx, "promote1"); err != nil {
		t.Errorf("cached copy became unusable after promotion: %v", err)
	}
}

// The exit criterion: a promoted item survives an eviction sweep, because its
// bytes now live under a library root rather than in the disposable cache.
func TestPromotedItemsSurviveEviction(t *testing.T) {
	h := newHarness(t, youtube.Options{})
	h.runWorkers()
	ctx := context.Background()

	h.svc.Prepare(ctx, youtube.SearchResult{VideoID: "safe1"})
	h.waitForState("safe1", dbgen.YtStateReady, 15*time.Second)

	if _, err := h.svc.Promote(ctx, "safe1"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	// Stand in for the scanner having ingested the file.
	var trackID uuid.UUID
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO library_roots (path) VALUES ('/promoted') RETURNING id`).Scan(&trackID); err != nil {
		t.Fatalf("insert root: %v", err)
	}
	var track uuid.UUID
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO tracks (root_id, rel_path, size, mtime, content_hash, suffix, title)
		VALUES ($1, 'safe.opus', 10, now(), '\x01'::bytea, 'opus', 'Safe')
		RETURNING id`, trackID).Scan(&track); err != nil {
		t.Fatalf("insert track: %v", err)
	}
	if err := h.svc.LinkPromoted(ctx, "safe1", track); err != nil {
		t.Fatalf("link promoted: %v", err)
	}

	// Expire it well past its TTL and sweep: without the promotion link this
	// would be removed.
	if _, err := h.pool.Exec(ctx,
		`UPDATE yt_items SET expires_at = now() - interval '1 day' WHERE video_id = 'safe1'`); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, err := h.svc.Evict(ctx); err != nil {
		t.Fatalf("evict: %v", err)
	}

	item, _ := h.svc.Get(ctx, "safe1")
	if item.State == dbgen.YtStateEvicted {
		t.Error("a promoted item was evicted")
	}
}

func TestPromoteRequiresACachedItem(t *testing.T) {
	h := newHarness(t, youtube.Options{})
	ctx := context.Background()

	h.svc.Prepare(ctx, youtube.SearchResult{VideoID: "notyet"})
	if _, err := h.svc.Promote(ctx, "notyet"); !errors.Is(err, youtube.ErrNotCached) {
		t.Errorf("err = %v, want ErrNotCached", err)
	}
	if _, err := h.svc.Promote(ctx, "unknown"); !errors.Is(err, youtube.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// Video titles routinely contain slashes and colons, which would otherwise
// escape the directory or produce an unwritable name.
func TestPromoteSanitisesPaths(t *testing.T) {
	h := newHarness(t, youtube.Options{})
	h.runWorkers()
	ctx := context.Background()

	h.svc.Prepare(ctx, youtube.SearchResult{VideoID: "nasty1"})
	h.waitForState("nasty1", dbgen.YtStateReady, 15*time.Second)

	if _, err := h.pool.Exec(ctx, `
		UPDATE yt_items SET title = $1, uploader = $2 WHERE video_id = 'nasty1'`,
		`../../etc/passwd: "Live"`, `../evil/Channel`); err != nil {
		t.Fatalf("set nasty metadata: %v", err)
	}

	path, err := h.svc.Promote(ctx, "nasty1")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	abs, _ := filepath.Abs(path)
	root, _ := filepath.Abs(h.dir)
	if !filepath.HasPrefix(abs, root) {
		t.Errorf("promoted to %q, which escapes the library root %q", abs, root)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("promoted file missing: %v", err)
	}
}
