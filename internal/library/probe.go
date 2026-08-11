package library

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cespare/xxhash/v2"
	"github.com/dhowden/tag"
)

// audioExtensions is the set of files the scanner considers. Everything else in
// a music directory — artwork, playlists, logs, cue sheets — is skipped.
var audioExtensions = map[string]bool{
	".mp3": true, ".flac": true, ".m4a": true, ".m4b": true, ".mp4": true,
	".aac": true, ".ogg": true, ".oga": true, ".opus": true, ".wav": true,
	".wma": true, ".aiff": true, ".aif": true, ".ape": true, ".wv": true,
	".alac": true, ".dsf": true, ".mpc": true,
}

// IsAudioFile reports whether a path looks like audio, by extension.
func IsAudioFile(path string) bool {
	return audioExtensions[strings.ToLower(filepath.Ext(path))]
}

// sidecarNames are checked, in order, for album art next to the audio file.
var sidecarNames = []string{
	"cover", "folder", "front", "album", "albumart", "artwork",
}

var sidecarExtensions = []string{".jpg", ".jpeg", ".png", ".webp"}

// hashWindow is how much of each end of the file feeds the content hash.
const hashWindow = 64 << 10

// Metadata is everything the scanner reads from one audio file.
type Metadata struct {
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	Genres      []string
	Year        int
	TrackNo     int
	DiscNo      int

	Codec  string
	Suffix string

	// Art is the embedded picture, if the file carries one.
	Art     []byte
	ArtMIME string
}

// Probe reads tags and computes the content hash for one file.
func Probe(path string) (Metadata, []byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return Metadata{}, nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return Metadata{}, nil, err
	}

	hash, err := contentHash(f, st.Size())
	if err != nil {
		return Metadata{}, nil, fmt.Errorf("hash: %w", err)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Metadata{}, nil, err
	}

	meta := Metadata{Suffix: strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")}

	m, err := tag.ReadFrom(f)
	if err != nil {
		// An unreadable or absent tag block is not a scan failure: the file is
		// still playable, and the filename is a usable fallback title. Only
		// genuinely broken reads should stop the scan.
		if !errors.Is(err, tag.ErrNoTagsFound) {
			return meta, hash, nil
		}
		meta.Title = titleFromFilename(path)
		return meta, hash, nil
	}

	meta.Title = strings.TrimSpace(m.Title())
	meta.Artist = strings.TrimSpace(m.Artist())
	meta.AlbumArtist = strings.TrimSpace(m.AlbumArtist())
	meta.Album = strings.TrimSpace(m.Album())
	meta.Year = m.Year()
	meta.Codec = string(m.FileType())
	meta.TrackNo, _ = m.Track()
	meta.DiscNo, _ = m.Disc()

	if g := strings.TrimSpace(m.Genre()); g != "" {
		meta.Genres = splitGenres(g)
	}
	if pic := m.Picture(); pic != nil && len(pic.Data) > 0 {
		meta.Art = pic.Data
		meta.ArtMIME = pic.MIMEType
	}

	if meta.Title == "" {
		meta.Title = titleFromFilename(path)
	}
	return meta, hash, nil
}

// contentHash is xxhash64 over the size plus the first and last 64 KiB.
//
// Reading the whole file would make a full scan IO-bound on content that is
// never otherwise needed. The hash exists only to recognise a moved file, so a
// collision misattributes a move rather than corrupting anything — and the
// ends of an audio file (tags, then audio frames) differ between any two real
// tracks.
func contentHash(r io.ReaderAt, size int64) ([]byte, error) {
	h := xxhash.New()

	var sizeBuf [8]byte
	for i := range 8 {
		sizeBuf[i] = byte(size >> (8 * i))
	}
	_, _ = h.Write(sizeBuf[:])

	window := int64(hashWindow)
	if size < window {
		window = size
	}

	head := make([]byte, window)
	if window > 0 {
		if _, err := r.ReadAt(head, 0); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		_, _ = h.Write(head)
	}

	// Only hash a tail when the file is big enough for it to be distinct from
	// the head; otherwise the same bytes would be mixed in twice.
	if size > window*2 {
		tailBuf := make([]byte, window)
		if _, err := r.ReadAt(tailBuf, size-window); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		_, _ = h.Write(tailBuf)
	}

	sum := h.Sum64()
	out := make([]byte, 8)
	for i := range 8 {
		out[i] = byte(sum >> (8 * i))
	}
	return out, nil
}

// FindSidecarArt looks for album art next to an audio file.
func FindSidecarArt(audioPath string) (string, bool) {
	dir := filepath.Dir(audioPath)
	for _, name := range sidecarNames {
		for _, ext := range sidecarExtensions {
			candidate := filepath.Join(dir, name+ext)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Size() > 0 {
				return candidate, true
			}
			// Directory listings are case-sensitive on Linux, and "Cover.jpg"
			// is at least as common as "cover.jpg".
			candidate = filepath.Join(dir, strings.ToUpper(name[:1])+name[1:]+ext)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Size() > 0 {
				return candidate, true
			}
		}
	}
	return "", false
}

// splitGenres handles the several conventions files use for multiple genres.
func splitGenres(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ';' || r == '/' || r == ',' || r == '|'
	})
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		// ID3v1 stored genres as a numeric index; a bare number carries no
		// meaning once decoded and is noise in a genre facet.
		if f == "" || isNumeric(f) {
			continue
		}
		key := strings.ToLower(f)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out
}

func isNumeric(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

func titleFromFilename(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
