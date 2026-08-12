package youtube

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// yt-dlp leaves thumbnail files beside the audio when embedding fails, so the
// output directory can hold audio.png and audio.webp as well. Matching on an
// "audio.*" prefix picked whichever sorted first, which for a .webm source is
// the PNG.
func TestFindAudioFileIgnoresThumbnails(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"audio.info.json", "audio.png", "audio.webp", "audio.webm"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	got, err := findAudioFile(dir)
	if err != nil {
		t.Fatalf("findAudioFile: %v", err)
	}
	if filepath.Base(got) != "audio.webm" {
		t.Errorf("found %q, want audio.webm", filepath.Base(got))
	}
}

func TestFindAudioFilePrefersOpus(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"audio.webm", "audio.opus"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	got, err := findAudioFile(dir)
	if err != nil {
		t.Fatalf("findAudioFile: %v", err)
	}
	if filepath.Base(got) != "audio.opus" {
		t.Errorf("found %q, want the converted audio.opus", filepath.Base(got))
	}
}

func TestFindAudioFileRejectsEmptyAndMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := findAudioFile(dir); err == nil {
		t.Error("an empty directory produced a file")
	}

	// A zero-byte file is a failed conversion, not a download.
	if err := os.WriteFile(filepath.Join(dir, "audio.opus"), nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := findAudioFile(dir); err == nil {
		t.Error("a zero-byte file was accepted as audio")
	}
}

// Video titles routinely contain path separators and control characters.
func TestSanitise(t *testing.T) {
	cases := map[string]string{
		"Normal Title":          "Normal Title",
		"AC/DC - Thunderstruck": "AC-DC - Thunderstruck",
		`Bad: "Quoted" <Title>`: "Bad- -Quoted- -Title-",
		"../../etc/passwd":      "..-..-etc-passwd",
		"":                      "Unknown",
		"   ":                   "Unknown",
		"...":                   "Unknown",
		"with\x00null":          "withnull",
	}
	for in, want := range cases {
		if got := sanitise(in); got != want {
			t.Errorf("sanitise(%q) = %q, want %q", in, got, want)
		}
	}
}

// Whatever the input, the result must be usable as a single path component.
func TestSanitiseNeverEscapesADirectory(t *testing.T) {
	for _, in := range []string{
		"../../../etc/passwd", "/absolute/path", `..\..\windows`,
		strings.Repeat("a", 500), ".", "..",
	} {
		got := sanitise(in)
		if got == "" {
			t.Errorf("sanitise(%q) produced an empty name", in)
		}
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("sanitise(%q) = %q, which contains a path separator", in, got)
		}
		if got == "." || got == ".." {
			t.Errorf("sanitise(%q) = %q, which resolves to a directory", in, got)
		}
		if len(got) > 120 {
			t.Errorf("sanitise(%q) is %d characters, over the limit", in, len(got))
		}
	}
}

// Leading dots are legitimate in titles and must survive; only a name made
// entirely of dots is a problem.
func TestSanitiseKeepsLeadingDots(t *testing.T) {
	if got := sanitise("...Baby One More Time"); got != "...Baby One More Time" {
		t.Errorf("sanitise stripped leading dots: %q", got)
	}
}
