//go:build integration

package api_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// seedStreamable writes a real file into a library root and registers it, so
// the streaming tests exercise the actual open-and-serve path rather than a
// stub. Returns the track id and the bytes on disk.
func (h *harness) seedStreamable(t *testing.T, name string, size int) (string, []byte) {
	t.Helper()
	ctx := context.Background()

	root := t.TempDir()
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % 251)
	}
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write audio file: %v", err)
	}

	rootID := "00000000-0000-4000-8000-00000000f001"
	trackID := "00000000-0000-4000-8000-00000000f002"
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO library_roots (id, path) VALUES ($1, $2)
		 ON CONFLICT (path) DO NOTHING`, rootID, root); err != nil {
		t.Fatalf("insert root: %v", err)
	}
	suffix := filepath.Ext(name)[1:]
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO tracks (id, root_id, rel_path, size, mtime, content_hash,
		                    suffix, title, duration_ms)
		VALUES ($1, $2, $3, $4, now(), '\x01'::bytea, $5, 'Streamable', 1000)`,
		trackID, rootID, name, int64(size), suffix); err != nil {
		t.Fatalf("insert track: %v", err)
	}
	return trackID, content
}

func (h *harness) rawGet(t *testing.T, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/api/v1"+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// ---- whole-file streaming --------------------------------------------------------

func TestStreamServesWholeFile(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()
	id, want := h.seedStreamable(t, "song.mp3", 8192)

	resp := h.rawGet(t, "/tracks/"+id+"/stream", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", got)
	}
	// Without this header a browser will not offer a seek bar at all.
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, want) {
		t.Errorf("served %d bytes, want the %d on disk", len(body), len(want))
	}
}

// Seeking in a browser is entirely a property of getting Range right.
func TestStreamHonoursRangeRequests(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()
	id, want := h.seedStreamable(t, "song.mp3", 8192)

	cases := []struct {
		name      string
		header    string
		wantStart int
		wantEnd   int
	}{
		{"leading bytes", "bytes=0-1023", 0, 1023},
		{"middle", "bytes=2048-4095", 2048, 4095},
		{"open-ended", "bytes=8000-", 8000, 8191},
		{"suffix", "bytes=-192", 8000, 8191},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.rawGet(t, "/tracks/"+id+"/stream", map[string]string{"Range": tc.header})
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusPartialContent {
				t.Fatalf("status = %d, want 206", resp.StatusCode)
			}
			wantRange := fmt.Sprintf("bytes %d-%d/%d", tc.wantStart, tc.wantEnd, len(want))
			if got := resp.Header.Get("Content-Range"); got != wantRange {
				t.Errorf("Content-Range = %q, want %q", got, wantRange)
			}
			body, _ := io.ReadAll(resp.Body)
			expected := want[tc.wantStart : tc.wantEnd+1]
			if !bytes.Equal(body, expected) {
				t.Errorf("body is %d bytes, want %d and matching the file",
					len(body), len(expected))
			}
			if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(expected)) {
				t.Errorf("Content-Length = %q, want %d", got, len(expected))
			}
		})
	}
}

func TestStreamRejectsUnsatisfiableRange(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()
	id, _ := h.seedStreamable(t, "song.mp3", 1024)

	resp := h.rawGet(t, "/tracks/"+id+"/stream", map[string]string{"Range": "bytes=99999-"})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("status = %d, want 416", resp.StatusCode)
	}
}

// Players issue HEAD to learn the length and whether ranges are supported
// before they start a ranged read.
func TestStreamAnswersHEAD(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()
	id, want := h.seedStreamable(t, "song.mp3", 4096)

	req, _ := http.NewRequest(http.MethodHead, h.server.URL+"/api/v1/tracks/"+id+"/stream", nil)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(want)) {
		t.Errorf("Content-Length = %q, want %d", got, len(want))
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("HEAD returned %d bytes of body", len(body))
	}
}

// A conditional re-request must be cheap: this is what stops a re-listen
// re-downloading the whole track.
func TestStreamSupportsConditionalRequests(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()
	id, _ := h.seedStreamable(t, "song.mp3", 4096)

	first := h.rawGet(t, "/tracks/"+id+"/stream", nil)
	first.Body.Close()
	etag := first.Header.Get("Etag")
	if etag == "" {
		t.Fatal("no ETag on the first response")
	}

	second := h.rawGet(t, "/tracks/"+id+"/stream", map[string]string{"If-None-Match": etag})
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d, want 304", second.StatusCode)
	}
}

// ---- content types ------------------------------------------------------------------

func TestStreamContentTypes(t *testing.T) {
	for suffix, want := range map[string]string{
		"mp3":  "audio/mpeg",
		"flac": "audio/flac",
		"m4a":  "audio/mp4",
		"ogg":  "audio/ogg",
		"opus": "audio/ogg",
		"wav":  "audio/wav",
	} {
		t.Run(suffix, func(t *testing.T) {
			h := newHarness(t)
			h.loginAsAdmin()
			id, _ := h.seedStreamable(t, "song."+suffix, 512)

			resp := h.rawGet(t, "/tracks/"+id+"/stream", nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if got := resp.Header.Get("Content-Type"); got != want {
				t.Errorf("Content-Type = %q, want %q", got, want)
			}
		})
	}
}

// ---- errors and authorisation ----------------------------------------------------------

func TestStreamRequiresAuthentication(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()
	id, _ := h.seedStreamable(t, "song.mp3", 512)

	// A fresh jar: audio URLs are reachable by anyone who knows them unless the
	// session cookie is checked, and <audio> cannot send an Authorization header.
	anon := newHarnessSharing(t, h)
	resp := anon.rawGet(t, "/tracks/"+id+"/stream", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestStreamUnknownTrackIs404(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	resp := h.rawGet(t, "/tracks/00000000-0000-4000-8000-00000000dead/stream", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestStreamMalformedIDIs422(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	resp := h.rawGet(t, "/tracks/not-a-uuid/stream", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
}

// A track whose file disappeared between the scan and the request must 404
// rather than 500.
func TestStreamMissingFileIs404(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()
	id, _ := h.seedStreamable(t, "song.mp3", 512)

	var root, rel string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT r.path, t.rel_path FROM tracks t JOIN library_roots r ON r.id = t.root_id
		 WHERE t.id = $1`, id).Scan(&root, &rel); err != nil {
		t.Fatalf("locate file: %v", err)
	}
	if err := os.Remove(filepath.Join(root, rel)); err != nil {
		t.Fatalf("remove file: %v", err)
	}

	resp := h.rawGet(t, "/tracks/"+id+"/stream", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// A track marked missing is not streamable even though its row still exists.
func TestStreamMissingTrackIs404(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()
	id, _ := h.seedStreamable(t, "song.mp3", 512)

	if _, err := h.pool.Exec(context.Background(),
		`UPDATE tracks SET missing_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("mark missing: %v", err)
	}

	resp := h.rawGet(t, "/tracks/"+id+"/stream", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ---- cover art -------------------------------------------------------------------------

func TestCoverArtIsServedAndRequiresAuth(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()
	ctx := context.Background()

	png := []byte{0x89, 'P', 'N', 'G', 1, 2, 3, 4}
	artID := "00000000-0000-4000-8000-00000000e001"
	key := "art/ab/abcdef.png"

	w, err := h.blobs.Create(ctx, key)
	if err != nil {
		t.Fatalf("create blob: %v", err)
	}
	w.Write(png)
	if err := w.Commit(); err != nil {
		t.Fatalf("commit blob: %v", err)
	}
	w.Close()

	if _, err := h.pool.Exec(ctx, `
		INSERT INTO cover_art (id, hash, blob_key, source, mime, bytes)
		VALUES ($1, '\x02'::bytea, $2, 'embedded', 'image/png', $3)`,
		artID, key, len(png)); err != nil {
		t.Fatalf("insert cover art: %v", err)
	}

	resp := h.rawGet(t, "/art/"+artID, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, png) {
		t.Error("served bytes do not match the stored blob")
	}

	anon := newHarnessSharing(t, h)
	unauth := anon.rawGet(t, "/art/"+artID, nil)
	defer unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous art request: status = %d, want 401", unauth.StatusCode)
	}
}

func TestUnknownCoverArtIs404(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	resp := h.rawGet(t, "/art/00000000-0000-4000-8000-00000000beef", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
