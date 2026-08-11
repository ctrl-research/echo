package blobstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Local {
	t.Helper()
	s, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	w, err := s.Create(ctx, "youtube/abc123.opus")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte("audio-bytes")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close after Commit: %v", err)
	}

	r, info, err := s.Open(ctx, "youtube/abc123.opus")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	if info.Size != 11 {
		t.Errorf("Size = %d, want 11", info.Size)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "audio-bytes" {
		t.Errorf("content = %q, want %q", got, "audio-bytes")
	}
}

// A writer abandoned without Commit must leave nothing behind — neither the
// final key nor a stray temp file that List would report as a real blob.
func TestAbandonedWriteLeavesNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	w, err := s.Create(ctx, "youtube/partial.opus")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte("half a file")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := s.Stat(ctx, "youtube/partial.opus"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat after abandoned write: err = %v, want ErrNotFound", err)
	}
	blobs, err := s.List(ctx, "youtube")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(blobs) != 0 {
		t.Errorf("List = %v, want empty", blobs)
	}
}

// An existing blob must stay readable and intact while a replacement is being
// written, and flip to the new content only at Commit.
func TestCreateDoesNotDisturbExistingUntilCommit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	w, _ := s.Create(ctx, "art/cover.jpg")
	w.Write([]byte("v1"))
	w.Commit()
	w.Close()

	w2, err := s.Create(ctx, "art/cover.jpg")
	if err != nil {
		t.Fatalf("Create replacement: %v", err)
	}
	if _, err := w2.Write([]byte("v2-longer")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	r, _, err := s.Open(ctx, "art/cover.jpg")
	if err != nil {
		t.Fatalf("Open during replacement: %v", err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "v1" {
		t.Errorf("content during replacement = %q, want %q", got, "v1")
	}

	if err := w2.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	w2.Close()

	r2, _, _ := s.Open(ctx, "art/cover.jpg")
	got2, _ := io.ReadAll(r2)
	r2.Close()
	if string(got2) != "v2-longer" {
		t.Errorf("content after commit = %q, want %q", got2, "v2-longer")
	}
}

func TestRejectsTraversal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := NewLocal(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	canary := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(canary, []byte("do not read"), 0o600); err != nil {
		t.Fatalf("write canary: %v", err)
	}

	for _, key := range []string{
		"../secret.txt",
		"youtube/../../secret.txt",
		"/../secret.txt",
		"",
		"a\x00b",
	} {
		if _, _, err := s.Open(ctx, key); err == nil {
			t.Errorf("Open(%q) succeeded, want error", key)
		}
		if _, err := s.Create(ctx, key); err == nil {
			t.Errorf("Create(%q) succeeded, want error", key)
		}
	}

	if _, err := os.ReadFile(canary); err != nil {
		t.Fatalf("canary damaged: %v", err)
	}
}

func TestDeleteMissingIsNotAnError(t *testing.T) {
	if err := newTestStore(t).Delete(context.Background(), "nope/missing.bin"); err != nil {
		t.Errorf("Delete(missing) = %v, want nil", err)
	}
}

func TestListEmptyPrefix(t *testing.T) {
	blobs, err := newTestStore(t).List(context.Background(), "transcode")
	if err != nil {
		t.Fatalf("List of absent prefix: %v", err)
	}
	if len(blobs) != 0 {
		t.Errorf("List = %v, want empty", blobs)
	}
}
