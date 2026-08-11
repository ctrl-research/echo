package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Local stores blobs on the filesystem beneath a root directory.
type Local struct {
	root string
}

var _ Store = (*Local)(nil)

// NewLocal creates the root directory if needed and returns a Store.
func NewLocal(root string) (*Local, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve blobstore root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create blobstore root: %w", err)
	}
	return &Local{root: abs}, nil
}

// resolve maps a key to an absolute path beneath the root.
//
// Keys reach this from user-influenced data (video IDs, content hashes), so
// traversal is rejected rather than sanitised. Sanitising would silently clamp
// "../x" to "x", making two distinct keys alias to one blob — in a
// content-addressed cache that means handing a caller the wrong audio.
//
// Non-canonical keys ("a//b", "a/./b") are rejected for the same reason: the
// key space must map injectively onto paths.
func (l *Local) resolve(key string) (string, error) {
	if key == "" {
		return "", errors.New("empty blob key")
	}
	if strings.ContainsRune(key, '\x00') {
		return "", fmt.Errorf("invalid blob key %q: contains NUL", key)
	}
	if strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("invalid blob key %q: must be relative", key)
	}
	if strings.ContainsRune(key, '\\') {
		return "", fmt.Errorf("invalid blob key %q: use '/' as separator", key)
	}

	clean := path.Clean(key)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid blob key %q: escapes root", key)
	}
	if clean != key {
		return "", fmt.Errorf("invalid blob key %q: not canonical (want %q)", key, clean)
	}

	full := filepath.Join(l.root, filepath.FromSlash(clean))
	// Defence in depth: the checks above should make this unreachable.
	if full != l.root && !strings.HasPrefix(full, l.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid blob key %q: escapes root", key)
	}
	return full, nil
}

func (l *Local) Open(_ context.Context, key string) (ReadSeekCloser, Info, error) {
	abs, err := l.resolve(key)
	if err != nil {
		return nil, Info{}, err
	}
	f, err := os.Open(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, Info{}, fmt.Errorf("%q: %w", key, ErrNotFound)
		}
		return nil, Info{}, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, Info{}, err
	}
	return f, Info{Key: key, Size: st.Size(), ModTime: st.ModTime()}, nil
}

func (l *Local) Create(_ context.Context, key string) (Writer, error) {
	abs, err := l.resolve(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, err
	}
	// Temp file in the destination directory so the final rename stays on one
	// filesystem and is therefore atomic.
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".tmp-*")
	if err != nil {
		return nil, err
	}
	return &localWriter{tmp: tmp, final: abs}, nil
}

func (l *Local) Stat(_ context.Context, key string) (Info, error) {
	abs, err := l.resolve(key)
	if err != nil {
		return Info{}, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Info{}, fmt.Errorf("%q: %w", key, ErrNotFound)
		}
		return Info{}, err
	}
	return Info{Key: key, Size: st.Size(), ModTime: st.ModTime()}, nil
}

func (l *Local) Delete(_ context.Context, key string) error {
	abs, err := l.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (l *Local) List(_ context.Context, prefix string) ([]Info, error) {
	base, err := l.resolve(prefix)
	if err != nil {
		return nil, err
	}
	var out []Info
	err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // an absent prefix is an empty listing, not a failure
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // raced with eviction
			}
			return err
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return err
		}
		out = append(out, Info{
			Key:     filepath.ToSlash(rel),
			Size:    st.Size(),
			ModTime: st.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (l *Local) LocalPath(key string) (string, bool) {
	abs, err := l.resolve(key)
	if err != nil {
		return "", false
	}
	return abs, true
}

type localWriter struct {
	tmp       *os.File
	final     string
	committed bool
}

var _ Writer = (*localWriter)(nil)

func (w *localWriter) Write(p []byte) (int, error) { return w.tmp.Write(p) }

func (w *localWriter) Commit() error {
	if w.committed {
		return errors.New("blob writer already committed")
	}
	// fsync before rename: a rename is atomic with respect to other readers,
	// but without the sync a crash can leave the entry pointing at a file
	// whose contents never reached disk.
	if err := w.tmp.Sync(); err != nil {
		return err
	}
	if err := w.tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(w.tmp.Name(), w.final); err != nil {
		return err
	}
	w.committed = true
	return nil
}

func (w *localWriter) Close() error {
	if w.committed {
		return nil
	}
	err := w.tmp.Close()
	if rmErr := os.Remove(w.tmp.Name()); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		err = errors.Join(err, rmErr)
	}
	return err
}

// Ensure io.Writer stays satisfied even if the interface grows.
var _ io.Writer = (*localWriter)(nil)
