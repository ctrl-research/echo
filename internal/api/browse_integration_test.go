//go:build integration

package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jonathanng/echo/internal/api"
)

// seedLibrary inserts a small library directly, so browse and search tests do
// not depend on the scanner. What is under test here is the read path.
func (h *harness) seedLibrary(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	_, err := h.pool.Exec(ctx, `
		INSERT INTO library_roots (id, path) VALUES
			('00000000-0000-4000-8000-000000000001', '/music');

		INSERT INTO artists (id, name, norm_name) VALUES
			('00000000-0000-4000-8000-0000000000a1', 'Radiohead',   'radiohead'),
			('00000000-0000-4000-8000-0000000000a2', 'Björk',       'bjork'),
			('00000000-0000-4000-8000-0000000000a3', 'The Beatles', 'beatles');

		INSERT INTO albums (id, name, norm_name, album_artist_id, year) VALUES
			('00000000-0000-4000-8000-0000000000b1', 'OK Computer', 'ok computer',
			 '00000000-0000-4000-8000-0000000000a1', 1997),
			('00000000-0000-4000-8000-0000000000b2', 'Homogenic', 'homogenic',
			 '00000000-0000-4000-8000-0000000000a2', 1997),
			('00000000-0000-4000-8000-0000000000b3', 'Abbey Road', 'abbey road',
			 '00000000-0000-4000-8000-0000000000a3', 1969);

		INSERT INTO genres (id, name) VALUES
			('00000000-0000-4000-8000-0000000000c1', 'Alternative Rock'),
			('00000000-0000-4000-8000-0000000000c2', 'Electronic'),
			('00000000-0000-4000-8000-0000000000c3', 'Rock');
	`)
	if err != nil {
		t.Fatalf("seed reference data: %v", err)
	}

	tracks := []struct {
		id, title, album, artist string
		trackNo, year            int
		genre                    string
	}{
		{"d1", "Airbag", "b1", "a1", 1, 1997, "c1"},
		{"d2", "Paranoid Android", "b1", "a1", 2, 1997, "c1"},
		{"d3", "Karma Police", "b1", "a1", 3, 1997, "c1"},
		{"d4", "Hunter", "b2", "a2", 1, 1997, "c2"},
		{"d5", "Jóga", "b2", "a2", 2, 1997, "c2"},
		{"d6", "Come Together", "b3", "a3", 1, 1969, "c3"},
		{"d7", "Something", "b3", "a3", 2, 1969, "c3"},
	}
	for i, tr := range tracks {
		id := "00000000-0000-4000-8000-0000000000" + tr.id
		_, err := h.pool.Exec(ctx, `
			INSERT INTO tracks (id, root_id, rel_path, size, mtime, content_hash,
			                    suffix, title, track_no, year, album_id, artist_id,
			                    album_artist_id, duration_ms)
			VALUES ($1, '00000000-0000-4000-8000-000000000001', $2, 1000, now(), $3,
			        'mp3', $4, $5, $6,
			        $7, $8, $8, 200000)`,
			id, "path/"+tr.id+".mp3", []byte{byte(i)}, tr.title, tr.trackNo, tr.year,
			"00000000-0000-4000-8000-0000000000"+tr.album,
			"00000000-0000-4000-8000-0000000000"+tr.artist)
		if err != nil {
			t.Fatalf("seed track %s: %v", tr.title, err)
		}
		if _, err := h.pool.Exec(ctx,
			`INSERT INTO track_genres (track_id, genre_id) VALUES ($1, $2)`,
			id, "00000000-0000-4000-8000-0000000000"+tr.genre); err != nil {
			t.Fatalf("seed genre link: %v", err)
		}
		// The search row is normally written by the scanner; the same SQL
		// definition is used here so search tests exercise the real index.
		if _, err := h.pool.Exec(ctx, `
			INSERT INTO track_search (track_id, haystack)
			SELECT te.id, concat_ws(' ', te.title, te.artist_name, te.album_name,
			              (SELECT string_agg(g.name::text, ' ') FROM track_genres tg
			               JOIN genres g ON g.id = tg.genre_id WHERE tg.track_id = te.id))
			FROM tracks_effective te WHERE te.id = $1`, id); err != nil {
			t.Fatalf("seed search row: %v", err)
		}
	}
}

type tracksResponse struct {
	Tracks     []api.TrackDTO `json:"tracks"`
	NextCursor string         `json:"nextCursor"`
}

// ---- browsing --------------------------------------------------------------------

func TestListTracks(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	resp := h.do(http.MethodGet, "/tracks", nil)
	assertStatus(t, resp, http.StatusOK)
	body := decode[tracksResponse](t, resp)

	if len(body.Tracks) != 7 {
		t.Fatalf("got %d tracks, want 7", len(body.Tracks))
	}
	// Default order is by title, case-insensitively.
	if body.Tracks[0].Title != "Airbag" {
		t.Errorf("first track = %q, want Airbag", body.Tracks[0].Title)
	}
	// Reconciled names come through the effective view, not raw columns.
	if body.Tracks[0].ArtistName != "Radiohead" {
		t.Errorf("artist = %q, want Radiohead", body.Tracks[0].ArtistName)
	}
	if len(body.Tracks[0].Genres) != 1 || body.Tracks[0].Genres[0] != "Alternative Rock" {
		t.Errorf("genres = %v, want [Alternative Rock]", body.Tracks[0].Genres)
	}
}

func TestListTracksFilters(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	cases := map[string]struct {
		query string
		want  int
	}{
		"by artist":                 {"?artist=00000000-0000-4000-8000-0000000000a1", 3},
		"by album":                  {"?album=00000000-0000-4000-8000-0000000000b2", 2},
		"by genre":                  {"?genre=Rock", 2},
		"by year":                   {"?year=1969", 2},
		"genre is case-insensitive": {"?genre=rock", 2},
		"combined":                  {"?artist=00000000-0000-4000-8000-0000000000a1&year=1997", 3},
		"no matches":                {"?year=1900", 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resp := h.do(http.MethodGet, "/tracks"+tc.query, nil)
			assertStatus(t, resp, http.StatusOK)
			body := decode[tracksResponse](t, resp)
			if len(body.Tracks) != tc.want {
				t.Errorf("got %d tracks, want %d", len(body.Tracks), tc.want)
			}
		})
	}
}

// Keyset pagination must walk the whole set exactly once: no gaps, no repeats.
func TestTrackPaginationCoversEverythingOnce(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	seen := map[string]int{}
	cursor := ""
	for pages := 0; pages < 20; pages++ {
		path := "/tracks?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		resp := h.do(http.MethodGet, path, nil)
		assertStatus(t, resp, http.StatusOK)
		body := decode[tracksResponse](t, resp)

		for _, tr := range body.Tracks {
			seen[tr.ID]++
		}
		if body.NextCursor == "" {
			break
		}
		cursor = body.NextCursor
	}

	if len(seen) != 7 {
		t.Errorf("paged over %d distinct tracks, want 7", len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("track %s appeared %d times across pages", id, count)
		}
	}
}

func TestMalformedCursorIsRejected(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	resp := h.do(http.MethodGet, "/tracks?cursor=not-a-real-cursor", nil)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()
}

func TestListAlbumsAndGetAlbum(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	resp := h.do(http.MethodGet, "/albums", nil)
	assertStatus(t, resp, http.StatusOK)
	albums := decode[struct {
		Albums []api.AlbumDTO `json:"albums"`
	}](t, resp)
	if len(albums.Albums) != 3 {
		t.Fatalf("got %d albums, want 3", len(albums.Albums))
	}
	if albums.Albums[0].Name != "Abbey Road" {
		t.Errorf("first album = %q, want Abbey Road", albums.Albums[0].Name)
	}
	if albums.Albums[0].TrackCount != 2 {
		t.Errorf("Abbey Road track count = %d, want 2", albums.Albums[0].TrackCount)
	}

	one := h.do(http.MethodGet, "/albums/00000000-0000-4000-8000-0000000000b1", nil)
	assertStatus(t, one, http.StatusOK)
	album := decode[struct {
		Album  api.AlbumDTO   `json:"album"`
		Tracks []api.TrackDTO `json:"tracks"`
	}](t, one)

	if album.Album.Name != "OK Computer" {
		t.Errorf("album = %q, want OK Computer", album.Album.Name)
	}
	if len(album.Tracks) != 3 {
		t.Fatalf("got %d tracks, want 3", len(album.Tracks))
	}
	// An album only makes sense in disc/track order, not alphabetical.
	for i, want := range []string{"Airbag", "Paranoid Android", "Karma Police"} {
		if album.Tracks[i].Title != want {
			t.Errorf("track %d = %q, want %q; album order is wrong",
				i, album.Tracks[i].Title, want)
		}
	}
}

func TestListArtistsAndGenres(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	resp := h.do(http.MethodGet, "/artists", nil)
	assertStatus(t, resp, http.StatusOK)
	artists := decode[struct {
		Artists []api.ArtistDTO `json:"artists"`
	}](t, resp)
	if len(artists.Artists) != 3 {
		t.Fatalf("got %d artists, want 3", len(artists.Artists))
	}
	// Sorted by normalised name, so "The Beatles" files under B.
	if artists.Artists[0].Name != "The Beatles" {
		t.Errorf("first artist = %q, want The Beatles (sorted on norm_name)",
			artists.Artists[0].Name)
	}
	if artists.Artists[0].TrackCount != 2 || artists.Artists[0].AlbumCount != 1 {
		t.Errorf("The Beatles counts = %d tracks / %d albums, want 2 / 1",
			artists.Artists[0].TrackCount, artists.Artists[0].AlbumCount)
	}

	genresResp := h.do(http.MethodGet, "/genres", nil)
	assertStatus(t, genresResp, http.StatusOK)
	genres := decode[struct {
		Genres []api.GenreDTO `json:"genres"`
	}](t, genresResp)
	if len(genres.Genres) != 3 {
		t.Fatalf("got %d genres, want 3", len(genres.Genres))
	}
	for _, g := range genres.Genres {
		if g.TrackCount == 0 {
			t.Errorf("genre %q has a zero track count", g.Name)
		}
	}
}

// ---- search -----------------------------------------------------------------------

type searchResponse struct {
	Tracks  []api.TrackDTO  `json:"tracks"`
	Albums  []api.AlbumDTO  `json:"albums"`
	Artists []api.ArtistDTO `json:"artists"`
	Fuzzy   bool            `json:"fuzzy"`
}

func TestSearchExactMatches(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	resp := h.do(http.MethodGet, "/search?q=radiohead", nil)
	assertStatus(t, resp, http.StatusOK)
	body := decode[searchResponse](t, resp)

	if len(body.Tracks) != 3 {
		t.Errorf("got %d tracks for 'radiohead', want 3", len(body.Tracks))
	}
	if len(body.Artists) == 0 {
		t.Error("no artists matched 'radiohead'")
	}
}

// The headline search property from the design: a typo still finds the artist.
func TestSearchFindsTypos(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	resp := h.do(http.MethodGet, "/search?q=radiohed", nil)
	assertStatus(t, resp, http.StatusOK)
	body := decode[searchResponse](t, resp)

	if len(body.Tracks) == 0 {
		t.Fatal("'radiohed' found nothing; the trigram fallback did not engage")
	}
	if !body.Fuzzy {
		t.Error("results are not flagged as fuzzy")
	}
	for _, tr := range body.Tracks {
		if tr.ArtistName != "Radiohead" {
			t.Errorf("unexpected track %q by %q", tr.Title, tr.ArtistName)
		}
	}
}

// Diacritics must fold in both directions: the index is unaccented, so an
// ASCII query has to reach an accented title.
func TestSearchFoldsDiacritics(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	for _, query := range []string{"bjork", "Björk", "joga"} {
		t.Run(query, func(t *testing.T) {
			resp := h.do(http.MethodGet, "/search?q="+query, nil)
			assertStatus(t, resp, http.StatusOK)
			body := decode[searchResponse](t, resp)
			if len(body.Tracks) == 0 && len(body.Artists) == 0 {
				t.Errorf("%q matched nothing", query)
			}
		})
	}
}

// "beatles" must reach "The Beatles": the article is stripped during
// normalisation, so the stored key and the folded query agree.
func TestSearchIgnoresLeadingArticle(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	resp := h.do(http.MethodGet, "/search?q=beatles", nil)
	assertStatus(t, resp, http.StatusOK)
	body := decode[searchResponse](t, resp)

	if len(body.Artists) == 0 {
		t.Fatal("'beatles' did not match 'The Beatles'")
	}
	if body.Artists[0].Name != "The Beatles" {
		t.Errorf("artist = %q, want The Beatles", body.Artists[0].Name)
	}
}

func TestSearchWithNoMatchesIsEmptyNotAnError(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	resp := h.do(http.MethodGet, "/search?q=zzzzzznothing", nil)
	assertStatus(t, resp, http.StatusOK)
	body := decode[searchResponse](t, resp)

	if len(body.Tracks) != 0 || len(body.Albums) != 0 || len(body.Artists) != 0 {
		t.Errorf("expected no results, got %+v", body)
	}
}

// ---- overrides ---------------------------------------------------------------------

// The core of the no-file-mutation design: a correction is stored in the
// database and applied at read time.
func TestOverridePersistsAndAppliesAtReadTime(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	const id = "00000000-0000-4000-8000-0000000000d1"

	patched := h.do(http.MethodPatch, "/tracks/"+id, map[string]any{
		"title": "Airbag (Remastered)",
		"year":  2017,
	})
	assertStatus(t, patched, http.StatusOK)
	updated := decode[api.TrackDTO](t, patched)

	if updated.Title != "Airbag (Remastered)" {
		t.Errorf("title = %q, want the corrected value", updated.Title)
	}
	if updated.Year != 2017 {
		t.Errorf("year = %d, want 2017", updated.Year)
	}
	if !updated.Overridden {
		t.Error("track is not flagged as overridden")
	}

	// The file's own tags are untouched: only the override row changed.
	var rawTitle string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT title FROM tracks WHERE id = $1`, id).Scan(&rawTitle); err != nil {
		t.Fatalf("read raw title: %v", err)
	}
	if rawTitle != "Airbag" {
		t.Errorf("the scanned title changed to %q; overrides must not touch it", rawTitle)
	}

	// And the correction survives a fresh read.
	again := h.do(http.MethodGet, "/tracks/"+id, nil)
	assertStatus(t, again, http.StatusOK)
	if got := decode[api.TrackDTO](t, again); got.Title != "Airbag (Remastered)" {
		t.Errorf("re-read title = %q, want the corrected value", got.Title)
	}
}

// A correction that is not reflected in the index produces a track that cannot
// be found by the name shown for it.
func TestOverrideUpdatesSearchIndex(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	const id = "00000000-0000-4000-8000-0000000000d1"
	patched := h.do(http.MethodPatch, "/tracks/"+id,
		map[string]any{"title": "Zeppelin Overture"})
	assertStatus(t, patched, http.StatusOK)
	patched.Body.Close()

	resp := h.do(http.MethodGet, "/search?q=zeppelin", nil)
	assertStatus(t, resp, http.StatusOK)
	body := decode[searchResponse](t, resp)

	if len(body.Tracks) == 0 {
		t.Fatal("the corrected title is not searchable")
	}
	if body.Tracks[0].Title != "Zeppelin Overture" {
		t.Errorf("found %q, want the corrected title", body.Tracks[0].Title)
	}
}

// A PATCH must change only what it names.
func TestOverrideIsPartial(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	const id = "00000000-0000-4000-8000-0000000000d1"

	first := h.do(http.MethodPatch, "/tracks/"+id, map[string]any{"title": "New Title"})
	assertStatus(t, first, http.StatusOK)
	first.Body.Close()

	second := h.do(http.MethodPatch, "/tracks/"+id, map[string]any{"year": 2020})
	assertStatus(t, second, http.StatusOK)
	got := decode[api.TrackDTO](t, second)

	if got.Title != "New Title" {
		t.Errorf("title = %q; the second PATCH cleared the first correction", got.Title)
	}
	if got.Year != 2020 {
		t.Errorf("year = %d, want 2020", got.Year)
	}
}

func TestClearingOverrideRevertsToFileTags(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	const id = "00000000-0000-4000-8000-0000000000d1"
	patched := h.do(http.MethodPatch, "/tracks/"+id, map[string]any{"title": "Wrong"})
	assertStatus(t, patched, http.StatusOK)
	patched.Body.Close()

	cleared := h.do(http.MethodDelete, "/tracks/"+id+"/override", nil)
	assertStatus(t, cleared, http.StatusNoContent)
	cleared.Body.Close()

	resp := h.do(http.MethodGet, "/tracks/"+id, nil)
	assertStatus(t, resp, http.StatusOK)
	got := decode[api.TrackDTO](t, resp)

	if got.Title != "Airbag" {
		t.Errorf("title = %q, want the file's own tag back", got.Title)
	}
	if got.Overridden {
		t.Error("track is still flagged as overridden")
	}

	// The index must revert too, or the discarded title stays searchable.
	search := h.do(http.MethodGet, "/search?q=wrong", nil)
	assertStatus(t, search, http.StatusOK)
	if body := decode[searchResponse](t, search); len(body.Tracks) != 0 {
		t.Error("the discarded title is still in the search index")
	}
}

// ---- authorisation -------------------------------------------------------------------

func TestLibraryEndpointsRequireAuthentication(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)

	for _, path := range []string{
		"/tracks", "/albums", "/artists", "/genres", "/search?q=x",
		"/tracks/00000000-0000-4000-8000-0000000000d1",
	} {
		resp := h.do(http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s: status = %d, want 401", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// A regular user browses and corrects; nothing here is admin-only.
func TestRegularUserCanBrowseAndCorrect(t *testing.T) {
	h := newHarness(t)
	h.seedLibrary(t)
	h.loginAsAdmin()

	created := h.do(http.MethodPost, "/admin/users", map[string]string{
		"email": "listener@example.com", "password": "listener-password", "role": "user",
	})
	assertStatus(t, created, http.StatusCreated)
	created.Body.Close()

	user := newHarnessSharing(t, h)
	resp := user.login("listener@example.com", "listener-password")
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	browsed := user.do(http.MethodGet, "/tracks", nil)
	assertStatus(t, browsed, http.StatusOK)
	browsed.Body.Close()

	patched := user.do(http.MethodPatch, "/tracks/00000000-0000-4000-8000-0000000000d1",
		map[string]any{"title": "User Correction"})
	assertStatus(t, patched, http.StatusOK)
	patched.Body.Close()

	// But admin surfaces stay closed to them.
	admin := user.do(http.MethodGet, "/admin/library", nil)
	assertStatus(t, admin, http.StatusForbidden)
	admin.Body.Close()
}
