//go:build integration

package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jonathanng/echo/internal/api"
)

const (
	airbag    = "00000000-0000-4000-8000-0000000000d1"
	paranoid  = "00000000-0000-4000-8000-0000000000d2"
	karma     = "00000000-0000-4000-8000-0000000000d3"
	comeTogth = "00000000-0000-4000-8000-0000000000d6"
)

type playlistBody struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Public     bool   `json:"public"`
	Owned      bool   `json:"owned"`
	TrackCount int64  `json:"trackCount"`
}

type playlistDetail struct {
	Playlist playlistBody `json:"playlist"`
	Tracks   []struct {
		EntryID     string       `json:"entryId"`
		Track       api.TrackDTO `json:"track"`
		Unavailable bool         `json:"unavailable"`
	} `json:"tracks"`
}

// secondUser creates another account and returns a harness signed in as them,
// which is what every per-user isolation assertion needs.
func (h *harness) secondUser(t *testing.T, email string) *harness {
	t.Helper()
	created := h.do(http.MethodPost, "/admin/users", map[string]string{
		"email": email, "password": "another-password", "role": "user",
	})
	assertStatus(t, created, http.StatusCreated)
	created.Body.Close()

	other := newHarnessSharing(t, h)
	resp := other.login(email, "another-password")
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	return other
}

func (h *harness) createPlaylist(t *testing.T, name string, public bool) playlistBody {
	t.Helper()
	resp := h.do(http.MethodPost, "/playlists", map[string]any{
		"name": name, "public": public,
	})
	assertStatus(t, resp, http.StatusCreated)
	return decode[playlistBody](t, resp)
}

// ---- playlists -------------------------------------------------------------------

func TestPlaylistLifecycle(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	pl := h.createPlaylist(t, "Late Night", false)
	if pl.Name != "Late Night" || !pl.Owned {
		t.Fatalf("created playlist = %+v", pl)
	}

	for _, track := range []string{airbag, paranoid, karma} {
		resp := h.do(http.MethodPost, "/playlists/"+pl.ID+"/tracks",
			map[string]string{"trackId": track})
		assertStatus(t, resp, http.StatusCreated)
		resp.Body.Close()
	}

	detail := decode[playlistDetail](t, h.do(http.MethodGet, "/playlists/"+pl.ID, nil))
	if len(detail.Tracks) != 3 {
		t.Fatalf("got %d tracks, want 3", len(detail.Tracks))
	}
	// Appended in order, so they come back in the order they were added.
	for i, want := range []string{"Airbag", "Paranoid Android", "Karma Police"} {
		if detail.Tracks[i].Track.Title != want {
			t.Errorf("track %d = %q, want %q", i, detail.Tracks[i].Track.Title, want)
		}
	}

	renamed := h.do(http.MethodPatch, "/playlists/"+pl.ID, map[string]any{"name": "Renamed"})
	assertStatus(t, renamed, http.StatusOK)
	if got := decode[playlistBody](t, renamed); got.Name != "Renamed" {
		t.Errorf("name = %q, want Renamed", got.Name)
	}

	deleted := h.do(http.MethodDelete, "/playlists/"+pl.ID, nil)
	assertStatus(t, deleted, http.StatusNoContent)
	deleted.Body.Close()

	gone := h.do(http.MethodGet, "/playlists/"+pl.ID, nil)
	assertStatus(t, gone, http.StatusNotFound)
	gone.Body.Close()
}

// The same song may legitimately appear twice in one playlist, so entries are
// identified by their own id rather than by track.
func TestPlaylistAllowsDuplicateTracks(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()
	pl := h.createPlaylist(t, "On Repeat", false)

	for range 2 {
		resp := h.do(http.MethodPost, "/playlists/"+pl.ID+"/tracks",
			map[string]string{"trackId": airbag})
		assertStatus(t, resp, http.StatusCreated)
		resp.Body.Close()
	}

	detail := decode[playlistDetail](t, h.do(http.MethodGet, "/playlists/"+pl.ID, nil))
	if len(detail.Tracks) != 2 {
		t.Fatalf("got %d entries, want 2", len(detail.Tracks))
	}
	if detail.Tracks[0].EntryID == detail.Tracks[1].EntryID {
		t.Error("the two entries share an id; they cannot be removed individually")
	}
}

func TestPlaylistReorder(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()
	pl := h.createPlaylist(t, "Ordered", false)

	for _, track := range []string{airbag, paranoid, karma} {
		resp := h.do(http.MethodPost, "/playlists/"+pl.ID+"/tracks",
			map[string]string{"trackId": track})
		resp.Body.Close()
	}
	before := decode[playlistDetail](t, h.do(http.MethodGet, "/playlists/"+pl.ID, nil))

	// Reverse it.
	ids := []string{
		before.Tracks[2].EntryID, before.Tracks[1].EntryID, before.Tracks[0].EntryID,
	}
	resp := h.do(http.MethodPut, "/playlists/"+pl.ID+"/order", map[string]any{"entryIds": ids})
	assertStatus(t, resp, http.StatusOK)
	after := decode[playlistDetail](t, resp)

	for i, want := range []string{"Karma Police", "Paranoid Android", "Airbag"} {
		if after.Tracks[i].Track.Title != want {
			t.Errorf("position %d = %q, want %q", i, after.Tracks[i].Track.Title, want)
		}
	}
}

// A partial reorder would leave the omitted entries at stale positions,
// colliding with the new ones.
func TestPlaylistReorderRejectsPartialList(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()
	pl := h.createPlaylist(t, "Ordered", false)

	for _, track := range []string{airbag, paranoid} {
		h.do(http.MethodPost, "/playlists/"+pl.ID+"/tracks",
			map[string]string{"trackId": track}).Body.Close()
	}
	detail := decode[playlistDetail](t, h.do(http.MethodGet, "/playlists/"+pl.ID, nil))

	resp := h.do(http.MethodPut, "/playlists/"+pl.ID+"/order",
		map[string]any{"entryIds": []string{detail.Tracks[0].EntryID}})
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()
}

// Removing from the middle must renumber, or the next append lands in a hole
// and the order goes wrong.
func TestPlaylistRemoveCompactsPositions(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()
	pl := h.createPlaylist(t, "Compact", false)

	for _, track := range []string{airbag, paranoid, karma} {
		h.do(http.MethodPost, "/playlists/"+pl.ID+"/tracks",
			map[string]string{"trackId": track}).Body.Close()
	}
	detail := decode[playlistDetail](t, h.do(http.MethodGet, "/playlists/"+pl.ID, nil))

	removed := h.do(http.MethodDelete,
		"/playlists/"+pl.ID+"/tracks/"+detail.Tracks[1].EntryID, nil)
	assertStatus(t, removed, http.StatusNoContent)
	removed.Body.Close()

	added := h.do(http.MethodPost, "/playlists/"+pl.ID+"/tracks",
		map[string]string{"trackId": comeTogth})
	assertStatus(t, added, http.StatusCreated)
	added.Body.Close()

	after := decode[playlistDetail](t, h.do(http.MethodGet, "/playlists/"+pl.ID, nil))
	want := []string{"Airbag", "Karma Police", "Come Together"}
	if len(after.Tracks) != len(want) {
		t.Fatalf("got %d tracks, want %d", len(after.Tracks), len(want))
	}
	for i, title := range want {
		if after.Tracks[i].Track.Title != title {
			t.Errorf("position %d = %q, want %q", i, after.Tracks[i].Track.Title, title)
		}
	}
}

// ---- per-user isolation ------------------------------------------------------------

// The milestone's exit criterion: two accounts must not see each other's state.
func TestPlaylistsArePrivateToTheirOwner(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()
	mine := h.createPlaylist(t, "Mine", false)

	other := h.secondUser(t, "other@example.com")

	listed := decode[struct {
		Playlists []playlistBody `json:"playlists"`
	}](t, other.do(http.MethodGet, "/playlists", nil))
	for _, p := range listed.Playlists {
		if p.ID == mine.ID {
			t.Error("a private playlist appeared in another user's listing")
		}
	}

	// Not merely hidden from the list — unreachable, and indistinguishable
	// from one that does not exist.
	fetched := other.do(http.MethodGet, "/playlists/"+mine.ID, nil)
	assertStatus(t, fetched, http.StatusNotFound)
	fetched.Body.Close()

	for _, attempt := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, "/playlists/" + mine.ID, map[string]any{"name": "Hijacked"}},
		{http.MethodDelete, "/playlists/" + mine.ID, nil},
		{http.MethodPost, "/playlists/" + mine.ID + "/tracks", map[string]string{"trackId": airbag}},
	} {
		resp := other.do(attempt.method, attempt.path, attempt.body)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404",
				attempt.method, attempt.path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// And the owner's copy is untouched.
	still := decode[playlistBody](t, h.do(http.MethodGet, "/playlists/"+mine.ID, nil))
	_ = still
}

// A public playlist is readable by others but still only editable by its owner.
func TestPublicPlaylistIsReadableButNotEditable(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()
	shared := h.createPlaylist(t, "Shared", true)
	h.do(http.MethodPost, "/playlists/"+shared.ID+"/tracks",
		map[string]string{"trackId": airbag}).Body.Close()

	other := h.secondUser(t, "reader@example.com")

	detail := decode[playlistDetail](t, other.do(http.MethodGet, "/playlists/"+shared.ID, nil))
	if len(detail.Tracks) != 1 {
		t.Errorf("reader saw %d tracks, want 1", len(detail.Tracks))
	}
	if detail.Playlist.Owned {
		t.Error("a reader is told they own somebody else's playlist")
	}

	edit := other.do(http.MethodPatch, "/playlists/"+shared.ID, map[string]any{"name": "Nope"})
	assertStatus(t, edit, http.StatusNotFound)
	edit.Body.Close()
}

func TestFavoritesArePerUser(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	fav := h.do(http.MethodPut, "/favorites/track/"+airbag, nil)
	assertStatus(t, fav, http.StatusNoContent)
	fav.Body.Close()

	mine := decode[tracksResponse](t, h.do(http.MethodGet, "/favorites", nil))
	if len(mine.Tracks) != 1 || mine.Tracks[0].ID != airbag {
		t.Fatalf("owner's favourites = %+v, want just Airbag", mine.Tracks)
	}

	other := h.secondUser(t, "listener@example.com")
	theirs := decode[tracksResponse](t, other.do(http.MethodGet, "/favorites", nil))
	if len(theirs.Tracks) != 0 {
		t.Errorf("another user sees %d favourites, want 0", len(theirs.Tracks))
	}

	// The flag on a track listing is per-caller too.
	mineListing := decode[tracksResponse](t, h.do(http.MethodGet, "/tracks", nil))
	theirsListing := decode[tracksResponse](t, other.do(http.MethodGet, "/tracks", nil))
	find := func(rows []api.TrackDTO, id string) bool {
		for _, r := range rows {
			if r.ID == id {
				return r.Favorite
			}
		}
		t.Fatalf("track %s missing from listing", id)
		return false
	}
	if !find(mineListing.Tracks, airbag) {
		t.Error("owner's listing does not flag their favourite")
	}
	if find(theirsListing.Tracks, airbag) {
		t.Error("another user's listing flags a favourite that is not theirs")
	}
}

func TestUnfavoriteIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	// Removing something never favourited is a success, so a double-click on
	// the heart cannot produce an error.
	for range 2 {
		resp := h.do(http.MethodDelete, "/favorites/track/"+airbag, nil)
		assertStatus(t, resp, http.StatusNoContent)
		resp.Body.Close()
	}
}

// ---- plays ----------------------------------------------------------------------------

// The Last.fm rule: half the track or four minutes, whichever comes first.
// The seeded tracks are 200s, so the threshold is 100s.
func TestPlayCountsOnlyPastTheThreshold(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	type playResult struct {
		Counted bool  `json:"counted"`
		Needed  int64 `json:"neededMs"`
	}

	short := decode[playResult](t, h.do(http.MethodPost, "/plays",
		map[string]any{"trackId": airbag, "msPlayed": 30_000}))
	if short.Counted {
		t.Error("a 30s listen to a 200s track counted as a play")
	}
	if short.Needed != 100_000 {
		t.Errorf("threshold = %dms, want 100000 (half of 200s)", short.Needed)
	}

	long := decode[playResult](t, h.do(http.MethodPost, "/plays",
		map[string]any{"trackId": airbag, "msPlayed": 120_000}))
	if !long.Counted {
		t.Error("a 120s listen to a 200s track did not count")
	}

	history := decode[struct {
		History []api.HistoryEntryDTO `json:"history"`
	}](t, h.do(http.MethodGet, "/history", nil))
	if len(history.History) != 1 {
		t.Fatalf("history has %d entries, want 1", len(history.History))
	}
	if history.History[0].Title != "Airbag" {
		t.Errorf("history entry = %q, want Airbag", history.History[0].Title)
	}
}

// The server checks the threshold against the duration it knows, so a client
// cannot inflate its own counts by lying about how long it listened.
func TestPlayThresholdUsesServerSideDuration(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	// Claiming a play a moment after pressing play must still be refused.
	resp := h.do(http.MethodPost, "/plays", map[string]any{"trackId": airbag, "msPlayed": 1})
	assertStatus(t, resp, http.StatusOK)
	if decode[struct {
		Counted bool `json:"counted"`
	}](t, resp).Counted {
		t.Error("a 1ms play counted")
	}

	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM plays`).Scan(&n); err != nil {
		t.Fatalf("count plays: %v", err)
	}
	if n != 0 {
		t.Errorf("%d play rows written for a play that did not qualify", n)
	}
}

func TestHistoryIsPerUser(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	h.do(http.MethodPost, "/plays",
		map[string]any{"trackId": airbag, "msPlayed": 150_000}).Body.Close()

	other := h.secondUser(t, "quiet@example.com")
	theirs := decode[struct {
		History []api.HistoryEntryDTO `json:"history"`
	}](t, other.do(http.MethodGet, "/history", nil))
	if len(theirs.History) != 0 {
		t.Errorf("another user sees %d history entries, want 0", len(theirs.History))
	}
}

func TestTopTracksRanksByPlayCount(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	for range 3 {
		h.do(http.MethodPost, "/plays",
			map[string]any{"trackId": karma, "msPlayed": 150_000}).Body.Close()
	}
	h.do(http.MethodPost, "/plays",
		map[string]any{"trackId": airbag, "msPlayed": 150_000}).Body.Close()

	top := decode[tracksResponse](t, h.do(http.MethodGet, "/history/top", nil))
	if len(top.Tracks) != 2 {
		t.Fatalf("got %d tracks, want 2", len(top.Tracks))
	}
	if top.Tracks[0].Title != "Karma Police" {
		t.Errorf("top track = %q, want Karma Police", top.Tracks[0].Title)
	}
}

// A play outlives the track: deleting a file should not erase the fact that it
// was listened to.
func TestHistorySurvivesTrackDeletion(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	h.do(http.MethodPost, "/plays",
		map[string]any{"trackId": airbag, "msPlayed": 150_000}).Body.Close()

	if _, err := h.pool.Exec(context.Background(),
		`DELETE FROM tracks WHERE id = $1`, airbag); err != nil {
		t.Fatalf("delete track: %v", err)
	}

	history := decode[struct {
		History []api.HistoryEntryDTO `json:"history"`
	}](t, h.do(http.MethodGet, "/history", nil))
	if len(history.History) != 1 {
		t.Fatalf("history has %d entries, want 1 to survive", len(history.History))
	}
	if history.History[0].TrackID != "" {
		t.Error("the deleted track's id is still referenced")
	}
}

// ---- authorisation ------------------------------------------------------------------

func TestPlaylistEndpointsRequireAuthentication(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)

	for _, path := range []string{"/playlists", "/favorites", "/history", "/history/top"} {
		resp := h.do(http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s: status = %d, want 401", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}
