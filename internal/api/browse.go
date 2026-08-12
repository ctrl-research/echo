package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jonathanng/echo/internal/auth"
	"github.com/jonathanng/echo/internal/db/dbgen"
)

// defaultLimit and maxLimit bound every listing. The cap matters more than it
// looks: without it a client can ask for the whole library in one response and
// turn a browse into a memory spike on both ends.
const (
	defaultLimit = 50
	maxLimit     = 200
)

// ---- DTOs ---------------------------------------------------------------------

type TrackDTO struct {
	ID          string   `json:"id" format:"uuid"`
	Title       string   `json:"title"`
	ArtistID    string   `json:"artistId,omitempty" required:"false"`
	ArtistName  string   `json:"artistName"`
	AlbumID     string   `json:"albumId,omitempty" required:"false"`
	AlbumName   string   `json:"albumName"`
	AlbumArtist string   `json:"albumArtist,omitempty" required:"false"`
	Genres      []string `json:"genres"`
	Year        int32    `json:"year,omitempty" required:"false"`
	TrackNo     int32    `json:"trackNo,omitempty" required:"false"`
	DiscNo      int32    `json:"discNo,omitempty" required:"false"`
	DurationMs  int32    `json:"durationMs,omitempty" required:"false"`
	Bitrate     int32    `json:"bitrate,omitempty" required:"false"`
	Suffix      string   `json:"suffix" doc:"File extension, without the dot"`
	CoverArtID  string   `json:"coverArtId,omitempty" required:"false"`
	// Overridden reports whether any field has a user correction applied.
	Overridden bool `json:"overridden"`
	// Favorite is per-caller: the same track is favourited for one user and
	// not another.
	Favorite bool `json:"favorite"`
}

type AlbumDTO struct {
	ID         string `json:"id" format:"uuid"`
	Name       string `json:"name"`
	ArtistID   string `json:"artistId,omitempty" required:"false"`
	ArtistName string `json:"artistName"`
	Year       int32  `json:"year,omitempty" required:"false"`
	TrackCount int64  `json:"trackCount"`
	DurationMs int64  `json:"durationMs"`
	CoverArtID string `json:"coverArtId,omitempty" required:"false"`
}

type ArtistDTO struct {
	ID         string `json:"id" format:"uuid"`
	Name       string `json:"name"`
	TrackCount int64  `json:"trackCount"`
	AlbumCount int64  `json:"albumCount"`
}

type GenreDTO struct {
	ID         string `json:"id" format:"uuid"`
	Name       string `json:"name"`
	TrackCount int64  `json:"trackCount"`
}

// ---- conversions ---------------------------------------------------------------

func uuidString(v pgtype.UUID) string {
	if !v.Valid {
		return ""
	}
	return uuid.UUID(v.Bytes).String()
}

func derefInt32(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}

// trackRow is the shape every track-returning query produces. Declaring it here
// keeps one conversion for all of them instead of one per query.
type trackRow struct {
	ID              uuid.UUID
	Title           string
	ArtistID        pgtype.UUID
	ArtistName      string
	AlbumID         pgtype.UUID
	AlbumName       string
	AlbumArtistName string
	Genres          []string
	Year            *int32
	TrackNo         *int32
	DiscNo          *int32
	DurationMs      *int32
	Bitrate         *int32
	Suffix          string
	CoverArtID      pgtype.UUID
}

func (r trackRow) dto(overridden bool) TrackDTO {
	genres := r.Genres
	if genres == nil {
		genres = []string{}
	}
	return TrackDTO{
		ID: r.ID.String(), Title: r.Title,
		ArtistID: uuidString(r.ArtistID), ArtistName: r.ArtistName,
		AlbumID: uuidString(r.AlbumID), AlbumName: r.AlbumName,
		AlbumArtist: r.AlbumArtistName, Genres: genres,
		Year: derefInt32(r.Year), TrackNo: derefInt32(r.TrackNo),
		DiscNo: derefInt32(r.DiscNo), DurationMs: derefInt32(r.DurationMs),
		Bitrate: derefInt32(r.Bitrate), Suffix: r.Suffix,
		CoverArtID: uuidString(r.CoverArtID), Overridden: overridden,
	}
}

func fromListRow(r dbgen.ListTracksRow) trackRow {
	return trackRow{
		ID: r.ID, Title: r.Title, ArtistID: r.ArtistID, ArtistName: r.ArtistName,
		AlbumID: r.AlbumID, AlbumName: r.AlbumName, AlbumArtistName: r.AlbumArtistName,
		Genres: r.Genres, Year: r.Year, TrackNo: r.TrackNo, DiscNo: r.DiscNo,
		DurationMs: r.DurationMs, Bitrate: r.Bitrate, Suffix: r.Suffix,
		CoverArtID: r.CoverArtID,
	}
}

// ---- registration ---------------------------------------------------------------

func (s *Server) registerBrowse() {
	huma.Register(s.API, huma.Operation{
		OperationID: "listTracks", Method: http.MethodGet, Path: "/tracks",
		Summary: "Browse tracks", Tags: []string{"library"},
		Description: "Filter by artist, album, genre, or year. Keyset-paginated.",
	}, s.handleListTracks)

	huma.Register(s.API, huma.Operation{
		OperationID: "getTrack", Method: http.MethodGet, Path: "/tracks/{id}",
		Summary: "Get one track", Tags: []string{"library"},
	}, s.handleGetTrack)

	huma.Register(s.API, huma.Operation{
		OperationID: "updateTrack", Method: http.MethodPatch, Path: "/tracks/{id}",
		Summary:     "Correct a track's metadata",
		Description: "Stored as an override; the audio file is never modified.",
		Tags:        []string{"library"},
	}, s.handleUpdateTrack)

	huma.Register(s.API, huma.Operation{
		OperationID: "clearTrackOverride", Method: http.MethodDelete, Path: "/tracks/{id}/override",
		Summary:       "Discard corrections and revert to the file's own tags",
		DefaultStatus: http.StatusNoContent, Tags: []string{"library"},
	}, s.handleClearOverride)

	huma.Register(s.API, huma.Operation{
		OperationID: "listAlbums", Method: http.MethodGet, Path: "/albums",
		Summary: "Browse albums", Tags: []string{"library"},
	}, s.handleListAlbums)

	huma.Register(s.API, huma.Operation{
		OperationID: "getAlbum", Method: http.MethodGet, Path: "/albums/{id}",
		Summary: "Get an album with its tracks", Tags: []string{"library"},
	}, s.handleGetAlbum)

	huma.Register(s.API, huma.Operation{
		OperationID: "listArtists", Method: http.MethodGet, Path: "/artists",
		Summary: "Browse artists", Tags: []string{"library"},
	}, s.handleListArtists)

	huma.Register(s.API, huma.Operation{
		OperationID: "getArtist", Method: http.MethodGet, Path: "/artists/{id}",
		Summary: "Get an artist", Tags: []string{"library"},
	}, s.handleGetArtist)

	huma.Register(s.API, huma.Operation{
		OperationID: "listGenres", Method: http.MethodGet, Path: "/genres",
		Summary: "List genres with track counts", Tags: []string{"library"},
	}, s.handleListGenres)

	huma.Register(s.API, huma.Operation{
		OperationID: "search", Method: http.MethodGet, Path: "/search",
		Summary:     "Search tracks, albums, and artists",
		Description: "Ranked full-text search, with a fuzzy fallback for typos.",
		Tags:        []string{"library"},
	}, s.handleSearch)
}

// ---- tracks ----------------------------------------------------------------------

type ListTracksInput struct {
	ArtistID string `query:"artist" format:"uuid" required:"false"`
	AlbumID  string `query:"album" format:"uuid" required:"false"`
	Genre    string `query:"genre" required:"false"`
	Year     int    `query:"year" required:"false"`
	Cursor   string `query:"cursor" required:"false"`
	Limit    int    `query:"limit" default:"50" minimum:"1" maximum:"200"`
}

type ListTracksOutput struct {
	Body struct {
		Tracks     []TrackDTO `json:"tracks"`
		NextCursor string     `json:"nextCursor,omitempty" doc:"Absent on the last page"`
	}
}

func (s *Server) handleListTracks(ctx context.Context, in *ListTracksInput) (*ListTracksOutput, error) {
	cur, err := decodeCursor(in.Cursor)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed cursor")
	}
	limit := clampLimit(in.Limit)

	params := dbgen.ListTracksParams{Limit: int32(limit + 1), UserID: auth.FromContext(ctx).UserID}
	if in.ArtistID != "" {
		id, err := uuid.Parse(in.ArtistID)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("Malformed artist id")
		}
		params.ArtistID = pgtype.UUID{Bytes: id, Valid: true}
	}
	if in.AlbumID != "" {
		id, err := uuid.Parse(in.AlbumID)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("Malformed album id")
		}
		params.AlbumID = pgtype.UUID{Bytes: id, Valid: true}
	}
	if in.Genre != "" {
		params.Genre = &in.Genre
	}
	if in.Year > 0 {
		y := int32(in.Year)
		params.Year = &y
	}
	if in.Cursor != "" {
		params.CursorSort = &cur.Sort
		params.CursorID = pgtype.UUID{Bytes: cur.ID, Valid: true}
	}

	rows, err := s.queries.ListTracks(ctx, params)
	if err != nil {
		s.deps.Log.Error("list tracks failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not list tracks")
	}

	rows, next := page(rows, limit, func(r dbgen.ListTracksRow) (string, uuid.UUID) {
		return strings.ToLower(r.Title), r.ID
	})

	out := &ListTracksOutput{}
	out.Body.Tracks = make([]TrackDTO, 0, len(rows))
	for _, r := range rows {
		dto := fromListRow(r).dto(false)
		dto.Favorite = r.Favorite
		out.Body.Tracks = append(out.Body.Tracks, dto)
	}
	out.Body.NextCursor = next
	return out, nil
}

type TrackIDInput struct {
	ID string `path:"id" format:"uuid"`
}

type TrackOutput struct {
	Body TrackDTO
}

func (s *Server) handleGetTrack(ctx context.Context, in *TrackIDInput) (*TrackOutput, error) {
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed track id")
	}
	row, err := s.queries.GetTrack(ctx, dbgen.GetTrackParams{
		ID: id, UserID: auth.FromContext(ctx).UserID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("No such track")
		}
		s.deps.Log.Error("get track failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not load track")
	}
	overridden, err := s.hasOverride(ctx, id)
	if err != nil {
		s.deps.Log.Warn("override lookup failed", "error", err)
	}

	tr := trackRow{
		ID: row.ID, Title: row.Title, ArtistID: row.ArtistID, ArtistName: row.ArtistName,
		AlbumID: row.AlbumID, AlbumName: row.AlbumName, AlbumArtistName: row.AlbumArtistName,
		Genres: row.Genres, Year: row.Year, TrackNo: row.TrackNo, DiscNo: row.DiscNo,
		DurationMs: row.DurationMs, Bitrate: row.Bitrate, Suffix: row.Suffix,
		CoverArtID: row.CoverArtID,
	}
	dto := tr.dto(overridden)
	dto.Favorite = row.Favorite
	return &TrackOutput{Body: dto}, nil
}

func (s *Server) hasOverride(ctx context.Context, trackID uuid.UUID) (bool, error) {
	var exists bool
	err := s.deps.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM track_overrides WHERE track_id = $1)`, trackID).Scan(&exists)
	return exists, err
}

// ---- overrides --------------------------------------------------------------------

type UpdateTrackInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		// Pointers so "absent" and "explicitly set" stay distinguishable: a
		// PATCH must be able to fix a title without clearing the year.
		Title       *string `json:"title,omitempty" maxLength:"500"`
		ArtistName  *string `json:"artistName,omitempty" maxLength:"500"`
		AlbumName   *string `json:"albumName,omitempty" maxLength:"500"`
		AlbumArtist *string `json:"albumArtist,omitempty" maxLength:"500"`
		Genre       *string `json:"genre,omitempty" maxLength:"200"`
		Year        *int    `json:"year,omitempty" minimum:"0" maximum:"3000"`
		TrackNo     *int    `json:"trackNo,omitempty" minimum:"0"`
		DiscNo      *int    `json:"discNo,omitempty" minimum:"0"`
	}
}

func (s *Server) handleUpdateTrack(ctx context.Context, in *UpdateTrackInput) (*TrackOutput, error) {
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed track id")
	}
	if _, err := s.queries.GetTrack(ctx, dbgen.GetTrackParams{
		ID: id, UserID: auth.FromContext(ctx).UserID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("No such track")
		}
		return nil, huma.Error500InternalServerError("Could not load track")
	}

	params := dbgen.UpsertTrackOverrideParams{TrackID: id}
	params.Title = in.Body.Title
	params.ArtistName = in.Body.ArtistName
	params.AlbumName = in.Body.AlbumName
	params.AlbumArtistName = in.Body.AlbumArtist
	params.Genre = in.Body.Genre
	if in.Body.Year != nil {
		v := int32(*in.Body.Year)
		params.Year = &v
	}
	if in.Body.TrackNo != nil {
		v := int32(*in.Body.TrackNo)
		params.TrackNo = &v
	}
	if in.Body.DiscNo != nil {
		v := int32(*in.Body.DiscNo)
		params.DiscNo = &v
	}

	// The override and the search row move together: a correction that is not
	// reflected in the index produces a track that cannot be found by the name
	// displayed for it.
	tx, err := s.deps.Pool.Begin(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("Could not save correction")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	if _, err := q.UpsertTrackOverride(ctx, params); err != nil {
		s.deps.Log.Error("upsert override failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not save correction")
	}
	if err := q.RebuildTrackSearch(ctx, id); err != nil {
		s.deps.Log.Error("rebuild search row failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not save correction")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, huma.Error500InternalServerError("Could not save correction")
	}

	return s.handleGetTrack(ctx, &TrackIDInput{ID: in.ID})
}

type NoContentOutput struct {
	Status int
}

func (s *Server) handleClearOverride(ctx context.Context, in *TrackIDInput) (*NoContentOutput, error) {
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed track id")
	}

	tx, err := s.deps.Pool.Begin(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("Could not clear corrections")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	if err := q.ClearTrackOverride(ctx, id); err != nil {
		s.deps.Log.Error("clear override failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not clear corrections")
	}
	if err := q.RebuildTrackSearch(ctx, id); err != nil {
		s.deps.Log.Error("rebuild search row failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not clear corrections")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, huma.Error500InternalServerError("Could not clear corrections")
	}
	return &NoContentOutput{Status: http.StatusNoContent}, nil
}

// ---- albums -----------------------------------------------------------------------

type ListAlbumsInput struct {
	ArtistID string `query:"artist" format:"uuid" required:"false"`
	Genre    string `query:"genre" required:"false"`
	Year     int    `query:"year" required:"false"`
	Cursor   string `query:"cursor" required:"false"`
	Limit    int    `query:"limit" default:"50" minimum:"1" maximum:"200"`
}

type ListAlbumsOutput struct {
	Body struct {
		Albums     []AlbumDTO `json:"albums"`
		NextCursor string     `json:"nextCursor,omitempty"`
	}
}

func (s *Server) handleListAlbums(ctx context.Context, in *ListAlbumsInput) (*ListAlbumsOutput, error) {
	cur, err := decodeCursor(in.Cursor)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed cursor")
	}
	limit := clampLimit(in.Limit)

	params := dbgen.ListAlbumsParams{Limit: int32(limit + 1)}
	if in.ArtistID != "" {
		id, err := uuid.Parse(in.ArtistID)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("Malformed artist id")
		}
		params.ArtistID = pgtype.UUID{Bytes: id, Valid: true}
	}
	if in.Genre != "" {
		params.Genre = &in.Genre
	}
	if in.Year > 0 {
		y := int32(in.Year)
		params.Year = &y
	}
	if in.Cursor != "" {
		params.CursorSort = &cur.Sort
		params.CursorID = pgtype.UUID{Bytes: cur.ID, Valid: true}
	}

	rows, err := s.queries.ListAlbums(ctx, params)
	if err != nil {
		s.deps.Log.Error("list albums failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not list albums")
	}
	rows, next := page(rows, limit, func(r dbgen.ListAlbumsRow) (string, uuid.UUID) {
		return strings.ToLower(r.Name), r.ID
	})

	out := &ListAlbumsOutput{}
	out.Body.Albums = make([]AlbumDTO, 0, len(rows))
	for _, r := range rows {
		out.Body.Albums = append(out.Body.Albums, AlbumDTO{
			ID: r.ID.String(), Name: r.Name, ArtistID: uuidString(r.ArtistID),
			ArtistName: r.ArtistName, Year: derefInt32(r.Year),
			TrackCount: r.TrackCount, DurationMs: r.DurationMs,
			CoverArtID: uuidString(r.CoverArtID),
		})
	}
	out.Body.NextCursor = next
	return out, nil
}

type AlbumIDInput struct {
	ID string `path:"id" format:"uuid"`
}

type GetAlbumOutput struct {
	Body struct {
		Album  AlbumDTO   `json:"album"`
		Tracks []TrackDTO `json:"tracks"`
	}
}

func (s *Server) handleGetAlbum(ctx context.Context, in *AlbumIDInput) (*GetAlbumOutput, error) {
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed album id")
	}
	album, err := s.queries.GetAlbum(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("No such album")
		}
		s.deps.Log.Error("get album failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not load album")
	}
	tracks, err := s.queries.ListAlbumTracks(ctx, dbgen.ListAlbumTracksParams{
		AlbumID: pgtype.UUID{Bytes: id, Valid: true},
		UserID:  auth.FromContext(ctx).UserID,
	})
	if err != nil {
		s.deps.Log.Error("list album tracks failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not load album tracks")
	}

	out := &GetAlbumOutput{}
	out.Body.Album = AlbumDTO{
		ID: album.ID.String(), Name: album.Name, ArtistID: uuidString(album.ArtistID),
		ArtistName: album.ArtistName, Year: derefInt32(album.Year),
		TrackCount: album.TrackCount, DurationMs: album.DurationMs,
		CoverArtID: uuidString(album.CoverArtID),
	}
	out.Body.Tracks = make([]TrackDTO, 0, len(tracks))
	for _, r := range tracks {
		tr := trackRow{
			ID: r.ID, Title: r.Title, ArtistID: r.ArtistID, ArtistName: r.ArtistName,
			AlbumID: r.AlbumID, AlbumName: r.AlbumName, AlbumArtistName: r.AlbumArtistName,
			Genres: r.Genres, Year: r.Year, TrackNo: r.TrackNo, DiscNo: r.DiscNo,
			DurationMs: r.DurationMs, Bitrate: r.Bitrate, Suffix: r.Suffix,
			CoverArtID: r.CoverArtID,
		}
		dto := tr.dto(false)
		dto.Favorite = r.Favorite
		out.Body.Tracks = append(out.Body.Tracks, dto)
	}
	return out, nil
}

// ---- artists ----------------------------------------------------------------------

type ListArtistsInput struct {
	Cursor string `query:"cursor" required:"false"`
	Limit  int    `query:"limit" default:"50" minimum:"1" maximum:"200"`
}

type ListArtistsOutput struct {
	Body struct {
		Artists    []ArtistDTO `json:"artists"`
		NextCursor string      `json:"nextCursor,omitempty"`
	}
}

func (s *Server) handleListArtists(ctx context.Context, in *ListArtistsInput) (*ListArtistsOutput, error) {
	cur, err := decodeCursor(in.Cursor)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed cursor")
	}
	limit := clampLimit(in.Limit)

	params := dbgen.ListArtistsParams{Limit: int32(limit + 1)}
	if in.Cursor != "" {
		params.CursorSort = &cur.Sort
		params.CursorID = pgtype.UUID{Bytes: cur.ID, Valid: true}
	}

	rows, err := s.queries.ListArtists(ctx, params)
	if err != nil {
		s.deps.Log.Error("list artists failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not list artists")
	}
	// Artists sort on norm_name, which the row does not carry; the same
	// normalisation the scanner applies reproduces it.
	rows, next := page(rows, limit, func(r dbgen.ListArtistsRow) (string, uuid.UUID) {
		return normalizeForCursor(r.Name), r.ID
	})

	out := &ListArtistsOutput{}
	out.Body.Artists = make([]ArtistDTO, 0, len(rows))
	for _, r := range rows {
		out.Body.Artists = append(out.Body.Artists, ArtistDTO{
			ID: r.ID.String(), Name: r.Name,
			TrackCount: r.TrackCount, AlbumCount: r.AlbumCount,
		})
	}
	out.Body.NextCursor = next
	return out, nil
}

type ArtistIDInput struct {
	ID string `path:"id" format:"uuid"`
}

type ArtistOutput struct {
	Body ArtistDTO
}

func (s *Server) handleGetArtist(ctx context.Context, in *ArtistIDInput) (*ArtistOutput, error) {
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed artist id")
	}
	row, err := s.queries.GetArtist(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("No such artist")
		}
		s.deps.Log.Error("get artist failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not load artist")
	}
	return &ArtistOutput{Body: ArtistDTO{
		ID: row.ID.String(), Name: row.Name,
		TrackCount: row.TrackCount, AlbumCount: row.AlbumCount,
	}}, nil
}

// ---- genres -----------------------------------------------------------------------

type ListGenresOutput struct {
	Body struct {
		Genres []GenreDTO `json:"genres"`
	}
}

func (s *Server) handleListGenres(ctx context.Context, _ *struct{}) (*ListGenresOutput, error) {
	rows, err := s.queries.ListGenres(ctx)
	if err != nil {
		s.deps.Log.Error("list genres failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not list genres")
	}
	out := &ListGenresOutput{}
	out.Body.Genres = make([]GenreDTO, 0, len(rows))
	for _, r := range rows {
		out.Body.Genres = append(out.Body.Genres, GenreDTO{
			ID: r.ID.String(), Name: r.Name, TrackCount: r.TrackCount,
		})
	}
	return out, nil
}

// ---- search -------------------------------------------------------------------------

type SearchInput struct {
	Query string `query:"q" minLength:"1" maxLength:"200"`
	Limit int    `query:"limit" default:"20" minimum:"1" maximum:"100"`
}

type SearchOutput struct {
	Body struct {
		Tracks  []TrackDTO  `json:"tracks"`
		Albums  []AlbumDTO  `json:"albums"`
		Artists []ArtistDTO `json:"artists"`
		// Fuzzy reports that exact matching found too little and the results
		// come from trigram similarity instead.
		Fuzzy bool `json:"fuzzy"`
	}
}

// fuzzyThreshold is how few exact hits it takes before falling back. Not zero:
// "bjor" legitimately matches nothing exactly while being an obvious prefix of
// something in the library, and a search that returns one weak hit is more
// annoying than one that widens.
const fuzzyThreshold = 3

func (s *Server) handleSearch(ctx context.Context, in *SearchInput) (*SearchOutput, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return nil, huma.Error422UnprocessableEntity("Search query is empty")
	}
	limit := int32(in.Limit)

	out := &SearchOutput{}
	out.Body.Tracks = []TrackDTO{}
	out.Body.Albums = []AlbumDTO{}
	out.Body.Artists = []ArtistDTO{}

	exact, err := s.queries.SearchTracksExact(ctx, dbgen.SearchTracksExactParams{
		Query: query, Limit: limit,
	})
	if err != nil {
		s.deps.Log.Error("exact search failed", "error", err)
		return nil, huma.Error500InternalServerError("Search failed")
	}

	if len(exact) >= fuzzyThreshold {
		for _, r := range exact {
			out.Body.Tracks = append(out.Body.Tracks, trackRow{
				ID: r.ID, Title: r.Title, ArtistID: r.ArtistID, ArtistName: r.ArtistName,
				AlbumID: r.AlbumID, AlbumName: r.AlbumName, AlbumArtistName: r.AlbumArtistName,
				Genres: r.Genres, Year: r.Year, TrackNo: r.TrackNo, DiscNo: r.DiscNo,
				DurationMs: r.DurationMs, Bitrate: r.Bitrate, Suffix: r.Suffix,
				CoverArtID: r.CoverArtID,
			}.dto(false))
		}
	} else {
		// Widen with trigram similarity, folding the query the same way the
		// stored haystack was folded so "bjork" can reach "Björk".
		fuzzy, err := s.queries.SearchTracksFuzzy(ctx, dbgen.SearchTracksFuzzyParams{
			Query: query, Limit: limit,
		})
		if err != nil {
			s.deps.Log.Error("fuzzy search failed", "error", err)
			return nil, huma.Error500InternalServerError("Search failed")
		}
		seen := map[uuid.UUID]bool{}
		for _, r := range exact {
			seen[r.ID] = true
			out.Body.Tracks = append(out.Body.Tracks, trackRow{
				ID: r.ID, Title: r.Title, ArtistID: r.ArtistID, ArtistName: r.ArtistName,
				AlbumID: r.AlbumID, AlbumName: r.AlbumName, AlbumArtistName: r.AlbumArtistName,
				Genres: r.Genres, Year: r.Year, TrackNo: r.TrackNo, DiscNo: r.DiscNo,
				DurationMs: r.DurationMs, Bitrate: r.Bitrate, Suffix: r.Suffix,
				CoverArtID: r.CoverArtID,
			}.dto(false))
		}
		for _, r := range fuzzy {
			if seen[r.ID] {
				continue
			}
			out.Body.Fuzzy = true
			out.Body.Tracks = append(out.Body.Tracks, trackRow{
				ID: r.ID, Title: r.Title, ArtistID: r.ArtistID, ArtistName: r.ArtistName,
				AlbumID: r.AlbumID, AlbumName: r.AlbumName, AlbumArtistName: r.AlbumArtistName,
				Genres: r.Genres, Year: r.Year, TrackNo: r.TrackNo, DiscNo: r.DiscNo,
				DurationMs: r.DurationMs, Bitrate: r.Bitrate, Suffix: r.Suffix,
				CoverArtID: r.CoverArtID,
			}.dto(false))
		}
	}

	// Albums and artists match on their normalised names, so a query has to be
	// folded the same way before it is compared.
	norm := normalizeForCursor(query)

	albums, err := s.queries.SearchAlbums(ctx, dbgen.SearchAlbumsParams{Query: norm, Limit: limit})
	if err != nil {
		s.deps.Log.Error("album search failed", "error", err)
	}
	for _, r := range albums {
		out.Body.Albums = append(out.Body.Albums, AlbumDTO{
			ID: r.ID.String(), Name: r.Name, ArtistName: r.ArtistName,
			Year: derefInt32(r.Year), CoverArtID: uuidString(r.CoverArtID),
		})
	}

	artists, err := s.queries.SearchArtists(ctx, dbgen.SearchArtistsParams{Query: norm, Limit: limit})
	if err != nil {
		s.deps.Log.Error("artist search failed", "error", err)
	}
	for _, r := range artists {
		out.Body.Artists = append(out.Body.Artists, ArtistDTO{ID: r.ID.String(), Name: r.Name})
	}

	return out, nil
}

func clampLimit(n int) int {
	if n <= 0 {
		return defaultLimit
	}
	return min(n, maxLimit)
}
