//go:build integration

package library_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jonathanng/echo/internal/blobstore"
	"github.com/jonathanng/echo/internal/db/dbgen"
	"github.com/jonathanng/echo/internal/dbtest"
	"github.com/jonathanng/echo/internal/library"
)

func TestMain(m *testing.M) { os.Exit(dbtest.Main(m)) }

type harness struct {
	t       *testing.T
	pool    *pgxpool.Pool
	q       *dbgen.Queries
	scanner *library.Scanner
	root    dbgen.LibraryRoot
	rootDir string
}

func newHarness(t *testing.T, specs ...trackSpec) *harness {
	t.Helper()

	pool := dbtest.New(t)
	blobs, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blobstore: %v", err)
	}

	rootDir := writeLibrary(t, specs...)
	scanner := library.NewScanner(pool, blobs, dbtest.DiscardLogger())

	roots, err := scanner.SyncRoots(context.Background(), []string{rootDir}, "")
	if err != nil {
		t.Fatalf("SyncRoots: %v", err)
	}
	return &harness{t: t, pool: pool, q: dbgen.New(pool), scanner: scanner,
		root: roots[0], rootDir: rootDir}
}

func (h *harness) scan() library.Result {
	h.t.Helper()
	result, err := h.scanner.ScanRoot(context.Background(), h.root.ID)
	if err != nil {
		h.t.Fatalf("scan: %v", err)
	}
	return result
}

func (h *harness) trackByPath(relPath string) dbgen.Track {
	h.t.Helper()
	track, err := h.q.GetTrackByPath(context.Background(), dbgen.GetTrackByPathParams{
		RootID: h.root.ID, RelPath: relPath,
	})
	if err != nil {
		h.t.Fatalf("track %q: %v", relPath, err)
	}
	return track
}

func (h *harness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		h.t.Fatalf("count: %v", err)
	}
	return n
}

// ---- basic ingestion ---------------------------------------------------------

func TestScanReadsTags(t *testing.T) {
	h := newHarness(t, trackSpec{
		RelPath: "Radiohead/OK Computer/01 Airbag.mp3",
		Title:   "Airbag", Artist: "Radiohead", Album: "OK Computer",
		Genre: "Alternative Rock", Year: 1997, TrackNo: 1,
	})

	result := h.scan()
	if result.Added != 1 || result.Seen != 1 {
		t.Fatalf("result = %+v, want 1 seen and 1 added", result)
	}

	track := h.trackByPath("Radiohead/OK Computer/01 Airbag.mp3")
	if track.Title != "Airbag" {
		t.Errorf("title = %q, want Airbag", track.Title)
	}
	if track.TrackNo == nil || *track.TrackNo != 1 {
		t.Errorf("track_no = %v, want 1", track.TrackNo)
	}
	if track.Year == nil || *track.Year != 1997 {
		t.Errorf("year = %v, want 1997", track.Year)
	}
	if track.Suffix != "mp3" {
		t.Errorf("suffix = %q, want mp3", track.Suffix)
	}
	if track.MissingAt.Valid {
		t.Error("a freshly scanned track is marked missing")
	}

	var artist string
	err := h.pool.QueryRow(context.Background(),
		`SELECT a.name FROM artists a JOIN tracks t ON t.artist_id = a.id WHERE t.id = $1`,
		track.ID).Scan(&artist)
	if err != nil {
		t.Fatalf("load artist: %v", err)
	}
	if artist != "Radiohead" {
		t.Errorf("artist = %q, want Radiohead", artist)
	}

	if n := h.count(`SELECT count(*) FROM genres`); n != 1 {
		t.Errorf("genre count = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM track_search WHERE track_id = $1`, track.ID); n != 1 {
		t.Errorf("search row count = %d, want 1", n)
	}
}

// Several formats, because tag parsing differs completely between them:
// ID3v2 frames, Vorbis comments, and MP4 atoms.
func TestScanHandlesMultipleFormats(t *testing.T) {
	// The non-mp3 fixtures carry fixed metadata, so the mp3 is tagged to match
	// and all four should reconcile onto one artist and one album.
	h := newHarness(t,
		trackSpec{RelPath: "a/one.mp3", Title: "One",
			Artist: fixtureArtist, Album: fixtureAlbum},
		trackSpec{RelPath: "a/two.flac"},
		trackSpec{RelPath: "a/three.m4a"},
		trackSpec{RelPath: "a/four.ogg"},
	)

	result := h.scan()
	if result.Added != 4 {
		t.Fatalf("added = %d, want 4 (result: %+v)", result.Added, result)
	}
	for _, rel := range []string{"a/two.flac", "a/three.m4a", "a/four.ogg"} {
		track := h.trackByPath(rel)
		if track.Title != fixtureTitle {
			t.Errorf("%s: title = %q, want %q; tags were not parsed",
				rel, track.Title, fixtureTitle)
		}
		if track.Year == nil || *track.Year != 2001 {
			t.Errorf("%s: year = %v, want 2001", rel, track.Year)
		}
	}
	// Reconciliation across formats: one artist and one album, not four each.
	if n := h.count(`SELECT count(*) FROM artists`); n != 1 {
		t.Errorf("artist count = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM albums`); n != 1 {
		t.Errorf("album count = %d, want 1", n)
	}
}

func TestScanIgnoresNonAudio(t *testing.T) {
	h := newHarness(t, trackSpec{RelPath: "a/song.mp3", Title: "Song", Artist: "A"})

	for _, name := range []string{"cover.jpg", "notes.txt", "playlist.m3u", "album.cue"} {
		if err := os.WriteFile(filepath.Join(h.rootDir, "a", name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	if result := h.scan(); result.Seen != 1 {
		t.Errorf("seen = %d, want 1; non-audio files were scanned", result.Seen)
	}
}

// Reconciliation is by normalised name, so tag variants must not fragment an
// artist into several rows.
func TestArtistVariantsReconcile(t *testing.T) {
	h := newHarness(t,
		trackSpec{RelPath: "a/1.mp3", Title: "One", Artist: "The Beatles", Album: "X"},
		trackSpec{RelPath: "a/2.mp3", Title: "Two", Artist: "Beatles", Album: "X"},
		trackSpec{RelPath: "a/3.mp3", Title: "Three", Artist: "THE BEATLES", Album: "X"},
	)
	h.scan()

	if n := h.count(`SELECT count(*) FROM artists`); n != 1 {
		t.Errorf("artist count = %d, want 1; name variants did not reconcile", n)
	}
}

// ---- incremental scanning ----------------------------------------------------

// The fast path: an unchanged file must not be re-probed. This is what makes a
// rescan of a large library take seconds.
func TestRescanSkipsUnchangedFiles(t *testing.T) {
	h := newHarness(t,
		trackSpec{RelPath: "a/1.mp3", Title: "One", Artist: "A", Album: "X"},
		trackSpec{RelPath: "a/2.mp3", Title: "Two", Artist: "A", Album: "X"},
	)

	if result := h.scan(); result.Added != 2 {
		t.Fatalf("first scan added = %d, want 2", result.Added)
	}

	second := h.scan()
	if second.Unchanged != 2 {
		t.Errorf("unchanged = %d, want 2 (result: %+v)", second.Unchanged, second)
	}
	if second.Added != 0 || second.Updated != 0 {
		t.Errorf("second scan re-ingested files: %+v", second)
	}
}

func TestRescanPicksUpEditedTags(t *testing.T) {
	h := newHarness(t, trackSpec{RelPath: "a/1.mp3", Title: "Before", Artist: "A", Album: "X"})
	h.scan()

	// Rewrite the file with a different title, which changes both its size
	// and its mtime.
	writeTrack(t, h.rootDir, trackSpec{
		RelPath: "a/1.mp3", Title: "After", Artist: "A", Album: "X",
	})

	result := h.scan()
	if result.Updated != 1 {
		t.Errorf("updated = %d, want 1 (result: %+v)", result.Updated, result)
	}
	if title := h.trackByPath("a/1.mp3").Title; title != "After" {
		t.Errorf("title = %q, want After", title)
	}
}

// The headline property: reorganising a library must not lose playlist entries
// or play history, which means a moved file keeps its track id.
func TestMovePreservesTrackID(t *testing.T) {
	h := newHarness(t, trackSpec{
		RelPath: "unsorted/song.mp3", Title: "Song", Artist: "A", Album: "X",
	})
	h.scan()
	before := h.trackByPath("unsorted/song.mp3")

	dest := filepath.Join(h.rootDir, "A", "X", "01 Song.mp3")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Rename(filepath.Join(h.rootDir, "unsorted", "song.mp3"), dest); err != nil {
		t.Fatalf("rename: %v", err)
	}

	result := h.scan()
	if result.Moved != 1 {
		t.Errorf("moved = %d, want 1 (result: %+v)", result.Moved, result)
	}

	after := h.trackByPath("A/X/01 Song.mp3")
	if after.ID != before.ID {
		t.Errorf("track id changed on move: %s → %s", before.ID, after.ID)
	}
	if after.MissingAt.Valid {
		t.Error("a moved track is marked missing")
	}
	if n := h.count(`SELECT count(*) FROM tracks`); n != 1 {
		t.Errorf("track count = %d, want 1; the move created a duplicate", n)
	}
}

// A deleted file is marked missing rather than removed, so playlists and
// history survive an unmounted drive.
func TestDeletedFileIsMarkedMissingNotDeleted(t *testing.T) {
	h := newHarness(t,
		trackSpec{RelPath: "a/1.mp3", Title: "One", Artist: "A", Album: "X"},
		trackSpec{RelPath: "a/2.mp3", Title: "Two", Artist: "A", Album: "X"},
	)
	h.scan()
	gone := h.trackByPath("a/1.mp3")

	if err := os.Remove(filepath.Join(h.rootDir, "a", "1.mp3")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	result := h.scan()
	if result.Missing != 1 {
		t.Errorf("missing = %d, want 1 (result: %+v)", result.Missing, result)
	}
	if n := h.count(`SELECT count(*) FROM tracks`); n != 2 {
		t.Errorf("track count = %d, want 2; the row was deleted rather than marked", n)
	}

	var missingAt *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT missing_at FROM tracks WHERE id = $1`, gone.ID).Scan(&missingAt); err != nil {
		t.Fatalf("load missing_at: %v", err)
	}
	if missingAt == nil {
		t.Error("missing_at was not set")
	}
}

// A file that comes back — a remounted drive — must be revived rather than
// left missing forever.
func TestRestoredFileIsUnmarked(t *testing.T) {
	h := newHarness(t, trackSpec{RelPath: "a/1.mp3", Title: "One", Artist: "A", Album: "X"})
	h.scan()

	path := filepath.Join(h.rootDir, "a", "1.mp3")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	h.scan()

	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatalf("restore: %v", err)
	}
	h.scan()

	if track := h.trackByPath("a/1.mp3"); track.MissingAt.Valid {
		t.Error("a restored file is still marked missing")
	}
}

// ---- cover art ----------------------------------------------------------------

// Album art is identical across an album's tracks, so content addressing must
// store it once rather than once per track.
func TestSidecarArtIsStoredOnce(t *testing.T) {
	h := newHarness(t,
		trackSpec{RelPath: "a/1.mp3", Title: "One", Artist: "A", Album: "X"},
		trackSpec{RelPath: "a/2.mp3", Title: "Two", Artist: "A", Album: "X"},
		trackSpec{RelPath: "a/3.mp3", Title: "Three", Artist: "A", Album: "X"},
	)

	// A 1x1 PNG is enough: the scanner stores bytes, it does not judge them.
	png := []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
		0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89,
		0, 0, 0, 0x0a, 'I', 'D', 'A', 'T',
		0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01,
		0x0d, 0x0a, 0x2d, 0xb4,
		0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
	}
	if err := os.WriteFile(filepath.Join(h.rootDir, "a", "cover.png"), png, 0o644); err != nil {
		t.Fatalf("write cover: %v", err)
	}

	h.scan()

	if n := h.count(`SELECT count(*) FROM cover_art`); n != 1 {
		t.Errorf("cover_art rows = %d, want 1; shared art was stored per track", n)
	}
	if n := h.count(`SELECT count(*) FROM tracks WHERE cover_art_id IS NOT NULL`); n != 3 {
		t.Errorf("%d of 3 tracks have art; the sidecar was not picked up", n)
	}
}

// ---- resilience ----------------------------------------------------------------

// One corrupt file must not abort a scan; the rest of the library still lands.
func TestCorruptFileDoesNotAbortScan(t *testing.T) {
	h := newHarness(t,
		trackSpec{RelPath: "a/good1.mp3", Title: "Good One", Artist: "A", Album: "X"},
		trackSpec{RelPath: "a/good2.mp3", Title: "Good Two", Artist: "A", Album: "X"},
	)
	if err := os.WriteFile(filepath.Join(h.rootDir, "a", "broken.mp3"),
		[]byte("this is not an mp3"), 0o644); err != nil {
		t.Fatalf("write broken file: %v", err)
	}

	result := h.scan()
	if result.Seen != 3 {
		t.Errorf("seen = %d, want 3", result.Seen)
	}
	// The good files must be present regardless of what happened to the third.
	for _, rel := range []string{"a/good1.mp3", "a/good2.mp3"} {
		h.trackByPath(rel)
	}
}

// A file with no tags at all is still playable, so it must still be indexed —
// with a title derived from the filename rather than an empty string.
func TestUntaggedFileFallsBackToFilename(t *testing.T) {
	h := newHarness(t)
	writeTrack(t, h.rootDir, trackSpec{RelPath: "loose/Some Recording.mp3"})

	if result := h.scan(); result.Added != 1 {
		t.Fatalf("added = %d, want 1", result.Added)
	}
	if title := h.trackByPath("loose/Some Recording.mp3").Title; title != "Some Recording" {
		t.Errorf("title = %q, want the filename stem", title)
	}
}

func TestHiddenAndJunkDirectoriesAreSkipped(t *testing.T) {
	h := newHarness(t, trackSpec{RelPath: "a/1.mp3", Title: "One", Artist: "A"})
	writeTrack(t, h.rootDir, trackSpec{RelPath: "@eaDir/thumb.mp3", Title: "Junk"})
	writeTrack(t, h.rootDir, trackSpec{RelPath: ".hidden/secret.mp3", Title: "Hidden"})

	if result := h.scan(); result.Seen != 1 {
		t.Errorf("seen = %d, want 1; junk directories were scanned", result.Seen)
	}
}

// ---- purge ---------------------------------------------------------------------

func TestPurgeRemovesLongMissingTracks(t *testing.T) {
	h := newHarness(t, trackSpec{RelPath: "a/1.mp3", Title: "One", Artist: "A", Album: "X"})
	h.scan()

	if err := os.Remove(filepath.Join(h.rootDir, "a", "1.mp3")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	h.scan()

	// Backdate so the grace period has elapsed.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE tracks SET missing_at = now() - interval '40 days'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	blobs, _ := blobstore.NewLocal(t.TempDir())
	svc := library.NewService(h.pool, nil, blobs, dbtest.DiscardLogger())
	n, err := svc.PurgeMissing(context.Background(), 30*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeMissing: %v", err)
	}
	if n != 1 {
		t.Errorf("purged = %d, want 1", n)
	}
	if got := h.count(`SELECT count(*) FROM tracks`); got != 0 {
		t.Errorf("track count = %d, want 0", got)
	}
	// The artist and album existed only for that track.
	if got := h.count(`SELECT count(*) FROM artists`); got != 0 {
		t.Errorf("orphaned artists remain: %d", got)
	}
}

// A track still within the grace period must survive, so a drive unmounted for
// an afternoon does not cost the library.
func TestPurgeSparesRecentlyMissingTracks(t *testing.T) {
	h := newHarness(t, trackSpec{RelPath: "a/1.mp3", Title: "One", Artist: "A", Album: "X"})
	h.scan()
	if err := os.Remove(filepath.Join(h.rootDir, "a", "1.mp3")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	h.scan()

	blobs, _ := blobstore.NewLocal(t.TempDir())
	svc := library.NewService(h.pool, nil, blobs, dbtest.DiscardLogger())
	n, err := svc.PurgeMissing(context.Background(), 30*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeMissing: %v", err)
	}
	if n != 0 {
		t.Errorf("purged = %d, want 0", n)
	}
	if got := h.count(`SELECT count(*) FROM tracks`); got != 1 {
		t.Errorf("track count = %d, want 1", got)
	}
}

// A copy has the same content hash as its source while the source is still
// there. Treating that as a move would relocate the original's row and leave
// the real file with none — the original would simply vanish from the library.
func TestCopyIsNotMistakenForAMove(t *testing.T) {
	h := newHarness(t, trackSpec{
		RelPath: "a/original.mp3", Title: "Original", Artist: "A", Album: "X",
	})
	h.scan()
	original := h.trackByPath("a/original.mp3")

	source, err := os.ReadFile(filepath.Join(h.rootDir, "a", "original.mp3"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(filepath.Join(h.rootDir, "a", "copy.mp3"), source, 0o644); err != nil {
		t.Fatalf("write copy: %v", err)
	}

	result := h.scan()
	if result.Moved != 0 {
		t.Errorf("moved = %d, want 0; a copy was treated as a move", result.Moved)
	}
	if result.Added != 1 {
		t.Errorf("added = %d, want 1 (result: %+v)", result.Added, result)
	}

	// Both files must be present, and the original must keep its identity.
	if got := h.trackByPath("a/original.mp3"); got.ID != original.ID {
		t.Errorf("the original's row moved: %s → %s", original.ID, got.ID)
	}
	h.trackByPath("a/copy.mp3")
	if n := h.count(`SELECT count(*) FROM tracks WHERE missing_at IS NULL`); n != 2 {
		t.Errorf("live track count = %d, want 2", n)
	}
}

// Variants that reconcile together share one row, so one of them supplies the
// display name. That choice must not depend on which scanner worker got there
// first: the same library should always present the same name.
func TestReconciledArtistPrefersTheFullerName(t *testing.T) {
	h := newHarness(t,
		trackSpec{RelPath: "a/1.mp3", Title: "One", Artist: "Beatles", Album: "X"},
		trackSpec{RelPath: "a/2.mp3", Title: "Two", Artist: "The Beatles", Album: "X"},
		trackSpec{RelPath: "a/3.mp3", Title: "Three", Artist: "BEATLES", Album: "X"},
	)
	h.scan()

	var name string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT name FROM artists`).Scan(&name); err != nil {
		t.Fatalf("load artist: %v", err)
	}
	if name != "The Beatles" {
		t.Errorf("artist display name = %q, want %q", name, "The Beatles")
	}
}
