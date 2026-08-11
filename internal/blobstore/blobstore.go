// Package blobstore abstracts storage of derived binary data: transcoded
// audio, cached YouTube downloads, and extracted cover art.
//
// Everything here is disposable and reconstructible from the library plus the
// database. Original library files are never written through this interface —
// they are read directly from their library root and never mutated.
//
// The interface exists so that a future multi-replica deployment can swap the
// local-disk implementation for shared object storage without touching call
// sites. See docs/design.md, "Scaling, honestly".
package blobstore

import (
	"context"
	"errors"
	"io"
	"time"
)

var ErrNotFound = errors.New("blob not found")

// Info describes a stored blob.
type Info struct {
	Key      string
	Size     int64
	ModTime  time.Time
	Accessed time.Time
}

// ReadSeekCloser is what http.ServeContent needs to answer Range requests.
type ReadSeekCloser interface {
	io.ReadSeeker
	io.Closer
}

// Writer accumulates a blob that becomes visible only on Commit. Callers must
// always call Close; Close without Commit discards the partial write.
//
// Atomicity is not optional here: a yt-dlp download killed halfway through
// must not leave a truncated file that later reads mistake for a complete one.
type Writer interface {
	io.Writer
	// Commit atomically publishes the blob under its key.
	Commit() error
	// Close releases resources, discarding the blob unless Commit succeeded.
	Close() error
}

// Store is the storage backend contract.
//
// Keys are slash-separated paths scoped by kind, e.g.
// "youtube/dQw4w9WgXcQ.opus" or "art/3f/3fa9c1....jpg".
type Store interface {
	// Open returns a seekable reader. Returns ErrNotFound if absent.
	Open(ctx context.Context, key string) (ReadSeekCloser, Info, error)

	// Create begins an atomic write. Any existing blob remains readable and
	// unchanged until Commit.
	Create(ctx context.Context, key string) (Writer, error)

	// Stat reports metadata without opening the blob.
	Stat(ctx context.Context, key string) (Info, error)

	// Delete removes a blob. Deleting a missing blob is not an error.
	Delete(ctx context.Context, key string) error

	// List enumerates blobs under a key prefix. Used by cache eviction, which
	// needs sizes and access times to apply an LRU policy.
	List(ctx context.Context, prefix string) ([]Info, error)

	// LocalPath returns a filesystem path for the blob when the backend has
	// one, allowing callers to hand an *os.File to http.ServeContent and let
	// the kernel do the work. Object-storage backends return ok=false and
	// callers fall back to Open.
	LocalPath(key string) (path string, ok bool)
}
