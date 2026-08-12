package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jonathanng/echo/internal/blobstore"
)

const (
	// Opus at 160k is transparent for the formats that need converting here
	// (WMA, WavPack, Musepack), and every target browser decodes it.
	transcodeBitrate = "160k"
	transcodeMIME    = "audio/ogg"
	transcodeExt     = ".opus"

	// A four-minute track converts in a second or two; the ceiling exists to
	// stop a pathological file pinning a worker forever.
	transcodeTimeout = 10 * time.Minute
)

// Transcoder converts non-native audio into Opus, once, into the blob store.
type Transcoder struct {
	blobs blobstore.Store
	log   *slog.Logger

	ffmpegPath string
	available  bool

	// inflight collapses concurrent requests for the same track. Without it,
	// pressing play twice starts two ffmpeg processes writing the same output,
	// and the second wastes CPU producing bytes that are already there.
	mu       sync.Mutex
	inflight map[uuid.UUID]*transcodeJob
}

type transcodeJob struct {
	done chan struct{}
	key  string
	err  error
}

func NewTranscoder(blobs blobstore.Store, log *slog.Logger) *Transcoder {
	t := &Transcoder{blobs: blobs, log: log, inflight: map[uuid.UUID]*transcodeJob{}}
	if path, err := exec.LookPath("ffmpeg"); err == nil {
		t.ffmpegPath, t.available = path, true
	} else {
		// Not fatal: the native path covers essentially every real library, and
		// a missing ffmpeg only means exotic formats return 415 rather than
		// bringing the server down.
		log.Warn("ffmpeg not found; files needing transcoding cannot be played",
			"error", err)
	}
	return t
}

func (t *Transcoder) Available() bool { return t.available }

// CacheKey is where a track's transcoded audio lives in the blob store.
func CacheKey(trackID uuid.UUID) string {
	return "transcode/" + trackID.String() + transcodeExt
}

// Ensure returns the cache key for a track's transcoded audio, converting it
// first if necessary.
func (t *Transcoder) Ensure(ctx context.Context, trackID uuid.UUID, sourcePath string) (string, error) {
	key := CacheKey(trackID)

	if _, err := t.blobs.Stat(ctx, key); err == nil {
		return key, nil
	} else if !errors.Is(err, blobstore.ErrNotFound) {
		return "", err
	}

	t.mu.Lock()
	if job, running := t.inflight[trackID]; running {
		t.mu.Unlock()
		select {
		case <-job.done:
			return job.key, job.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	job := &transcodeJob{done: make(chan struct{}), key: key}
	t.inflight[trackID] = job
	t.mu.Unlock()

	// Deliberately not tied to the requesting context: a listener who navigates
	// away mid-conversion should not throw away the work, since the next
	// request wants exactly the same bytes.
	convertCtx, cancel := context.WithTimeout(context.Background(), transcodeTimeout)
	defer cancel()

	job.err = t.convert(convertCtx, key, sourcePath)

	t.mu.Lock()
	delete(t.inflight, trackID)
	t.mu.Unlock()
	close(job.done)

	return job.key, job.err
}

func (t *Transcoder) convert(ctx context.Context, key, sourcePath string) error {
	if !t.available {
		return ErrUnsupported
	}
	start := time.Now()

	// Written to a temp file rather than piped: ffmpeg's Ogg muxer seeks back
	// to fix up the header, which a pipe cannot do.
	tmp, err := os.CreateTemp("", "echo-transcode-*"+transcodeExt)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	cmd := exec.CommandContext(ctx, t.ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", sourcePath,
		"-vn", // drop embedded artwork; it is served separately
		"-c:a", "libopus", "-b:a", transcodeBitrate,
		"-map_metadata", "0",
		tmpPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, out)
	}

	converted, err := os.Open(tmpPath)
	if err != nil {
		return err
	}
	defer converted.Close()

	w, err := t.blobs.Create(ctx, key)
	if err != nil {
		return err
	}
	defer w.Close()

	if _, err := io.Copy(w, converted); err != nil {
		return fmt.Errorf("store transcode: %w", err)
	}
	if err := w.Commit(); err != nil {
		return fmt.Errorf("commit transcode: %w", err)
	}

	t.log.Info("transcoded track", "source", sourcePath,
		"duration", time.Since(start).Round(time.Millisecond))
	return nil
}
