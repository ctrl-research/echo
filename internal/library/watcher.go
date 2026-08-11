package library

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
)

// debounce is how long a path must stay quiet before it is enqueued.
//
// Copying a file produces a burst of write events, and a large FLAC can take
// seconds to land. Reacting to the first event would read a half-written file;
// waiting for quiet reads it once, complete.
const debounce = 3 * time.Second

// EnqueueFunc is called with a settled path. The watcher does no database work
// itself so it stays testable without one.
type EnqueueFunc func(rootID uuid.UUID, absPath string)

// Watcher reports filesystem changes under a set of roots.
//
// It must run as a singleton. Several watchers on one share would each observe
// every event and enqueue duplicate work — the job queue's dedupe_key limits
// the damage, but does not make it correct. See docs/design.md, "Scaling,
// honestly".
type Watcher struct {
	log     *slog.Logger
	enqueue EnqueueFunc

	fsw *fsnotify.Watcher

	mu      sync.Mutex
	pending map[string]*time.Timer
	roots   map[string]uuid.UUID // watched dir → owning root
}

func NewWatcher(log *slog.Logger, enqueue EnqueueFunc) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		log:     log,
		enqueue: enqueue,
		fsw:     fsw,
		pending: map[string]*time.Timer{},
		roots:   map[string]uuid.UUID{},
	}, nil
}

// Add registers a root and every directory beneath it.
//
// fsnotify is not recursive on any platform Echo targets, so each directory
// needs its own watch, and new directories must be registered as they appear.
func (w *Watcher) Add(rootID uuid.UUID, rootPath string) error {
	return filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			w.log.Warn("watch: skipping unreadable path", "path", path, "error", err)
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		if path != rootPath && shouldSkipDir(info.Name()) {
			return filepath.SkipDir
		}
		w.addDir(rootID, path)
		return nil
	})
}

func (w *Watcher) addDir(rootID uuid.UUID, path string) {
	if err := w.fsw.Add(path); err != nil {
		// Hitting the inotify watch limit is the usual cause on Linux, and it
		// degrades to "changes are picked up by the next full scan" rather than
		// breaking anything.
		w.log.Warn("watch: could not add directory", "path", path, "error", err)
		return
	}
	w.mu.Lock()
	w.roots[path] = rootID
	w.mu.Unlock()
}

// Run processes events until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	defer w.fsw.Close()
	w.log.Info("filesystem watcher started")

	for {
		select {
		case <-ctx.Done():
			w.cancelPending()
			return

		case event, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handle(event)

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.log.Warn("watch error", "error", err)
		}
	}
}

func (w *Watcher) handle(event fsnotify.Event) {
	rootID, ok := w.rootFor(event.Name)
	if !ok {
		return
	}

	// A new directory needs its own watch, and anything already inside it will
	// have been created before the watch existed — so scan it rather than wait
	// for events that already happened.
	if event.Has(fsnotify.Create) {
		if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
			if !shouldSkipDir(info.Name()) {
				if err := w.Add(rootID, event.Name); err != nil {
					w.log.Warn("watch: could not add new directory", "path", event.Name, "error", err)
				}
				w.walkNewDir(rootID, event.Name)
			}
			return
		}
	}

	if !IsAudioFile(event.Name) {
		return
	}
	// Chmod alone never changes content; acting on it would re-probe a library
	// every time a backup tool touched permissions.
	if event.Op == fsnotify.Chmod {
		return
	}
	w.schedule(rootID, event.Name)
}

// walkNewDir enqueues audio files that appeared with a freshly created
// directory, which a move or an unpack produces in one step.
func (w *Watcher) walkNewDir(rootID uuid.UUID, dir string) {
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if IsAudioFile(path) {
			w.schedule(rootID, path)
		}
		return nil
	})
}

// schedule (re)starts the quiet timer for a path.
func (w *Watcher) schedule(rootID uuid.UUID, path string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if timer, ok := w.pending[path]; ok {
		timer.Reset(debounce)
		return
	}
	w.pending[path] = time.AfterFunc(debounce, func() {
		w.mu.Lock()
		delete(w.pending, path)
		w.mu.Unlock()
		w.enqueue(rootID, path)
	})
}

func (w *Watcher) rootFor(path string) (uuid.UUID, bool) {
	dir := filepath.Dir(path)
	w.mu.Lock()
	defer w.mu.Unlock()
	id, ok := w.roots[dir]
	return id, ok
}

func (w *Watcher) cancelPending() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for path, timer := range w.pending {
		timer.Stop()
		delete(w.pending, path)
	}
}
