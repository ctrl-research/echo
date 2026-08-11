//go:build integration

package library_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/bogem/id3v2/v2"
)

// Fixtures are real encoded audio committed under testdata/, tagged in Go at
// test time.
//
// An earlier version shelled out to ffmpeg and skipped when it was absent —
// which silently turned this whole suite into a no-op on any machine without
// it. Real files plus a real tag writer keep the tests hermetic: they exercise
// actual ID3v2 frames, Vorbis comments, and MP4 atoms with no external tool.
//
// testdata/silence.mp3 is untagged, so each test can write whatever metadata it
// needs onto a copy. The tagged.* fixtures carry fixed metadata for the formats
// Go cannot easily write, and exist to prove each container format parses.
const (
	fixtureTitle  = "Fixture Song"
	fixtureArtist = "Fixture Artist"
	fixtureAlbum  = "Fixture Album"
)

// trackSpec describes one file to materialise.
type trackSpec struct {
	RelPath     string
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	Genre       string
	Year        int
	TrackNo     int
	DiscNo      int
}

// writeLibrary materialises specs into a temp directory and returns its path.
func writeLibrary(t *testing.T, specs ...trackSpec) string {
	t.Helper()
	root := t.TempDir()
	for _, spec := range specs {
		writeTrack(t, root, spec)
	}
	return root
}

// writeTrack copies the right fixture into place and applies the spec's tags.
func writeTrack(t *testing.T, root string, spec trackSpec) string {
	t.Helper()

	dest := filepath.Join(root, filepath.FromSlash(spec.RelPath))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ext := filepath.Ext(dest)
	source := "silence.mp3"
	if ext != ".mp3" {
		source = "tagged" + ext
	}
	copyFixture(t, source, dest)

	// Only MP3 gets per-test tags; the other formats carry the fixture's own.
	if ext == ".mp3" {
		tagMP3(t, dest, spec)
	}
	return dest
}

func copyFixture(t *testing.T, name, dest string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", dest, err)
	}
}

func tagMP3(t *testing.T, path string, spec trackSpec) {
	t.Helper()

	// An untagged file is a deliberate case — the scanner must fall back to the
	// filename — so an empty spec writes nothing at all.
	if spec.Title == "" && spec.Artist == "" && spec.Album == "" && spec.Genre == "" {
		return
	}

	tag, err := id3v2.Open(path, id3v2.Options{Parse: false})
	if err != nil {
		t.Fatalf("open %s for tagging: %v", path, err)
	}
	defer tag.Close()

	tag.SetVersion(4)
	if spec.Title != "" {
		tag.SetTitle(spec.Title)
	}
	if spec.Artist != "" {
		tag.SetArtist(spec.Artist)
	}
	if spec.Album != "" {
		tag.SetAlbum(spec.Album)
	}
	if spec.Genre != "" {
		tag.SetGenre(spec.Genre)
	}
	if spec.Year > 0 {
		tag.SetYear(strconv.Itoa(spec.Year))
	}
	if spec.AlbumArtist != "" {
		tag.AddTextFrame(tag.CommonID("Band/Orchestra/Accompaniment"),
			tag.DefaultEncoding(), spec.AlbumArtist)
	}
	if spec.TrackNo > 0 {
		tag.AddTextFrame(tag.CommonID("Track number/Position in set"),
			tag.DefaultEncoding(), strconv.Itoa(spec.TrackNo))
	}
	if spec.DiscNo > 0 {
		tag.AddTextFrame(tag.CommonID("Part of a set"),
			tag.DefaultEncoding(), strconv.Itoa(spec.DiscNo))
	}

	if err := tag.Save(); err != nil {
		t.Fatalf("save tags to %s: %v", path, err)
	}
}
