// Package youtube searches YouTube and caches audio from it.
//
// Everything that shells out to yt-dlp sits behind the Downloader interface.
// That boundary is deliberate: the real implementation depends on a binary that
// YouTube breaks every few weeks and on network access that CI must not have,
// while everything worth testing — the state machine, the sliding TTL, LRU
// eviction, promotion — is on this side of it and is tested with a fake.
package youtube

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrUnavailable means yt-dlp is not installed.
var ErrUnavailable = errors.New("youtube: yt-dlp is not available")

// SearchResult is one entry from a search.
type SearchResult struct {
	VideoID      string `json:"videoId"`
	Title        string `json:"title"`
	Uploader     string `json:"uploader"`
	DurationMs   int64  `json:"durationMs"`
	ThumbnailURL string `json:"thumbnailUrl"`
}

// DownloadResult describes a completed download.
type DownloadResult struct {
	// Path is a temporary file the caller takes ownership of.
	Path       string
	Title      string
	Uploader   string
	DurationMs int64
	Bytes      int64
}

// Downloader is the yt-dlp boundary.
type Downloader interface {
	Available() bool
	Search(ctx context.Context, query string, limit int) ([]SearchResult, error)
	Download(ctx context.Context, videoID string) (DownloadResult, error)
	Version(ctx context.Context) string
}

// CLI drives the real yt-dlp binary.
type CLI struct {
	path      string
	available bool
	log       *slog.Logger
	// cookiesFile is passed to yt-dlp when set. YouTube increasingly demands
	// cookies from datacenter addresses; a residential homelab usually does not
	// need them.
	cookiesFile string
	timeout     time.Duration
}

var _ Downloader = (*CLI)(nil)

func NewCLI(log *slog.Logger, cookiesFile string) *CLI {
	c := &CLI{log: log, cookiesFile: cookiesFile, timeout: 15 * time.Minute}
	if path, err := exec.LookPath("yt-dlp"); err == nil {
		c.path, c.available = path, true
	} else {
		log.Warn("yt-dlp not found; YouTube search and playback are unavailable",
			"error", err)
	}
	return c
}

func (c *CLI) Available() bool { return c.available }

func (c *CLI) Version(ctx context.Context) string {
	if !c.available {
		return ""
	}
	out, err := exec.CommandContext(ctx, c.path, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// searchEntry is the subset of yt-dlp's JSON this package consumes. The tool
// emits a large object per result; naming only what is used keeps a change on
// their side from breaking decoding here.
type searchEntry struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Uploader  string  `json:"uploader"`
	Channel   string  `json:"channel"`
	Duration  float64 `json:"duration"`
	Thumbnail string  `json:"thumbnail"`
}

// Search runs a metadata-only query. --flat-playlist keeps it to roughly one
// round trip rather than resolving every result's formats.
func (c *CLI) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if !c.available {
		return nil, ErrUnavailable
	}
	if limit <= 0 {
		limit = 20
	}

	args := []string{
		"--flat-playlist", "--dump-json", "--no-warnings", "--ignore-errors",
		fmt.Sprintf("ytsearch%d:%s", limit, query),
	}
	args = append(c.commonArgs(), args...)

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start yt-dlp: %w", err)
	}

	// One JSON object per line, streamed. A long default scanner buffer because
	// a single entry's metadata comfortably exceeds 64 KiB.
	results := make([]SearchResult, 0, limit)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var entry searchEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue // a malformed line is one bad result, not a failed search
		}
		if entry.ID == "" {
			continue
		}
		uploader := entry.Uploader
		if uploader == "" {
			uploader = entry.Channel
		}
		results = append(results, SearchResult{
			VideoID:      entry.ID,
			Title:        entry.Title,
			Uploader:     uploader,
			DurationMs:   int64(entry.Duration * 1000),
			ThumbnailURL: entry.Thumbnail,
		})
	}

	if err := cmd.Wait(); err != nil {
		// Partial results are still useful: --ignore-errors means a few
		// unavailable videos should not fail the whole search.
		if len(results) == 0 {
			return nil, fmt.Errorf("yt-dlp search: %w: %s", err, tail(stderr.String()))
		}
		c.log.Warn("yt-dlp search reported errors but returned results",
			"error", err, "results", len(results))
	}
	return results, nil
}

// Download fetches the best audio stream and converts it to Opus.
//
// bestaudio is the raw audio track, so there is no ad insertion on this path at
// all — ads exist in the video delivery layer, not in the audio format. The
// sponsorblock flags handle the different problem of host-read sponsor segments
// baked into the audio itself.
func (c *CLI) Download(ctx context.Context, videoID string) (DownloadResult, error) {
	if !c.available {
		return DownloadResult{}, ErrUnavailable
	}

	dir, err := os.MkdirTemp("", "echo-yt-*")
	if err != nil {
		return DownloadResult{}, err
	}
	// The caller takes ownership of the audio file, so only the directory is
	// cleaned up here — after the file has been moved out.
	defer os.RemoveAll(dir)

	output := filepath.Join(dir, "audio.%(ext)s")
	// --write-info-json writes alongside the output template.
	infoPath := filepath.Join(dir, "audio.info.json")

	args := append(c.commonArgs(),
		"-f", "bestaudio/best",
		"--extract-audio", "--audio-format", "opus", "--audio-quality", "0",
		"--embed-metadata", "--embed-thumbnail",
		"--sponsorblock-remove", "sponsor,intro,outro,selfpromo,music_offtopic",
		"--no-playlist", "--no-warnings",
		"--write-info-json",
		"-o", output,
		"https://www.youtube.com/watch?v="+videoID,
	)

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.path, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	// The audio file is the thing that matters. yt-dlp exits non-zero when any
	// post-processor fails — embedding a thumbnail, say — and those are
	// decoration. Discarding a perfectly good download over one would mean
	// every download fails on a host missing an optional dependency.
	audio, err := findAudioFile(dir)
	if err != nil {
		if runErr != nil {
			return DownloadResult{}, fmt.Errorf("yt-dlp download: %w: %s", runErr, tail(stderr.String()))
		}
		return DownloadResult{}, err
	}
	if runErr != nil {
		c.log.Warn("yt-dlp reported an error but produced audio; continuing",
			"video", videoID, "error", runErr, "stderr", tail(stderr.String()))
	}
	info, err := os.Stat(audio)
	if err != nil {
		return DownloadResult{}, err
	}

	result := DownloadResult{Path: audio, Bytes: info.Size()}
	if meta, err := readInfoJSON(infoPath); err == nil {
		result.Title = meta.Title
		result.Uploader = meta.Uploader
		if result.Uploader == "" {
			result.Uploader = meta.Channel
		}
		result.DurationMs = int64(meta.Duration * 1000)
	}

	// Moved out of the temp directory this function is about to delete.
	final := filepath.Join(os.TempDir(), "echo-yt-"+videoID+filepath.Ext(audio))
	if err := os.Rename(audio, final); err != nil {
		// Rename fails across filesystems; fall back to a copy.
		if err := copyFile(audio, final); err != nil {
			return DownloadResult{}, err
		}
	}
	result.Path = final
	return result, nil
}

// commonArgs are the flags every invocation shares.
func (c *CLI) commonArgs() []string {
	args := []string{"--no-progress", "--no-color", "--socket-timeout", "30"}
	if c.cookiesFile != "" {
		args = append(args, "--cookies", c.cookiesFile)
	}
	return args
}

// audioExtensions are what --audio-format may leave behind, in preference
// order. Matching by extension rather than by an "audio.*" prefix matters: the
// same directory also holds audio.png and audio.webp from thumbnail handling,
// and a .webm source sorts after the PNG.
var audioExtensions = []string{".opus", ".m4a", ".mp3", ".ogg", ".webm", ".aac", ".flac"}

func findAudioFile(dir string) (string, error) {
	for _, ext := range audioExtensions {
		candidate := filepath.Join(dir, "audio"+ext)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Size() > 0 {
			return candidate, nil
		}
	}
	return "", errors.New("youtube: yt-dlp produced no audio file")
}

type infoJSON struct {
	Title    string  `json:"title"`
	Uploader string  `json:"uploader"`
	Channel  string  `json:"channel"`
	Duration float64 `json:"duration"`
}

func readInfoJSON(path string) (infoJSON, error) {
	var out infoJSON
	raw, err := os.ReadFile(path)
	if err != nil {
		// yt-dlp writes this alongside the audio; a missing file costs metadata
		// but not the download.
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o644)
}

// tail trims a long stderr dump to the part that usually carries the reason.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 500 {
		return s
	}
	return "…" + s[len(s)-500:]
}
