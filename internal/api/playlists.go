package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jonathanng/echo/internal/auth"
	"github.com/jonathanng/echo/internal/db/dbgen"
)

// scrobbleFloor is the shorter of the two thresholds a play must reach to
// count: half the track, or four minutes.
//
// This is Last.fm's rule, adopted so counts stay comparable if history is ever
// forwarded to an external service. Counting on track start would let skipping
// through an album inflate everything; counting only on completion would drop
// anything stopped twenty seconds early.
const (
	scrobbleFraction = 0.5
	scrobbleCeiling  = 4 * time.Minute
)

func (s *Server) registerPlaylists() {
	reg := func(id, method, path, summary string, status int) huma.Operation {
		return huma.Operation{
			OperationID: id, Method: method, Path: path,
			Summary: summary, DefaultStatus: status, Tags: []string{"library"},
		}
	}

	huma.Register(s.API, reg("listPlaylists", http.MethodGet, "/playlists",
		"List playlists", http.StatusOK), s.handleListPlaylists)
	huma.Register(s.API, reg("createPlaylist", http.MethodPost, "/playlists",
		"Create a playlist", http.StatusCreated), s.handleCreatePlaylist)
	huma.Register(s.API, reg("getPlaylist", http.MethodGet, "/playlists/{id}",
		"Get a playlist with its tracks", http.StatusOK), s.handleGetPlaylist)
	huma.Register(s.API, reg("updatePlaylist", http.MethodPatch, "/playlists/{id}",
		"Rename or reshare a playlist", http.StatusOK), s.handleUpdatePlaylist)
	huma.Register(s.API, reg("deletePlaylist", http.MethodDelete, "/playlists/{id}",
		"Delete a playlist", http.StatusNoContent), s.handleDeletePlaylist)
	huma.Register(s.API, reg("addPlaylistTrack", http.MethodPost, "/playlists/{id}/tracks",
		"Append a track", http.StatusCreated), s.handleAddPlaylistTrack)
	huma.Register(s.API, reg("removePlaylistTrack", http.MethodDelete,
		"/playlists/{id}/tracks/{entryId}", "Remove an entry", http.StatusNoContent),
		s.handleRemovePlaylistTrack)
	huma.Register(s.API, reg("reorderPlaylist", http.MethodPut, "/playlists/{id}/order",
		"Reorder a playlist", http.StatusOK), s.handleReorderPlaylist)

	huma.Register(s.API, reg("listFavorites", http.MethodGet, "/favorites",
		"List favourited tracks", http.StatusOK), s.handleListFavorites)
	huma.Register(s.API, reg("addFavorite", http.MethodPut, "/favorites/{type}/{id}",
		"Mark a favourite", http.StatusNoContent), s.handleAddFavorite)
	huma.Register(s.API, reg("removeFavorite", http.MethodDelete, "/favorites/{type}/{id}",
		"Unmark a favourite", http.StatusNoContent), s.handleRemoveFavorite)

	huma.Register(s.API, reg("recordPlay", http.MethodPost, "/plays",
		"Report a completed play", http.StatusOK), s.handleRecordPlay)
	huma.Register(s.API, reg("listHistory", http.MethodGet, "/history",
		"Recently played", http.StatusOK), s.handleListHistory)
	huma.Register(s.API, reg("topTracks", http.MethodGet, "/history/top",
		"Most played tracks", http.StatusOK), s.handleTopTracks)
}

// ---- DTOs ----------------------------------------------------------------------

type PlaylistDTO struct {
	ID          string    `json:"id" format:"uuid"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Public      bool      `json:"public"`
	Owned       bool      `json:"owned" doc:"Whether the caller may modify it"`
	OwnerName   string    `json:"ownerName"`
	TrackCount  int64     `json:"trackCount"`
	DurationMs  int64     `json:"durationMs"`
	CreatedAt   time.Time `json:"createdAt"`
}

type PlaylistEntryDTO struct {
	EntryID string   `json:"entryId" format:"uuid" doc:"Identifies this entry, not the track"`
	Track   TrackDTO `json:"track"`
	// Unavailable marks an entry whose file has gone missing. Kept in the
	// listing rather than hidden, so the owner can see what happened.
	Unavailable bool `json:"unavailable"`
}

// ---- playlists ------------------------------------------------------------------

type ListPlaylistsOutput struct {
	Body struct {
		Playlists []PlaylistDTO `json:"playlists"`
	}
}

func (s *Server) handleListPlaylists(ctx context.Context, _ *struct{}) (*ListPlaylistsOutput, error) {
	me := auth.FromContext(ctx)
	rows, err := s.queries.ListPlaylists(ctx, me.UserID)
	if err != nil {
		s.deps.Log.Error("list playlists failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not list playlists")
	}
	out := &ListPlaylistsOutput{}
	out.Body.Playlists = make([]PlaylistDTO, 0, len(rows))
	for _, p := range rows {
		out.Body.Playlists = append(out.Body.Playlists, PlaylistDTO{
			ID: p.ID.String(), Name: p.Name, Description: p.Description,
			Public: p.Public, Owned: p.Owned, OwnerName: p.OwnerName,
			TrackCount: p.TrackCount, DurationMs: p.DurationMs, CreatedAt: p.CreatedAt,
		})
	}
	return out, nil
}

type CreatePlaylistInput struct {
	Body struct {
		Name        string `json:"name" minLength:"1" maxLength:"200"`
		Description string `json:"description,omitempty" maxLength:"2000" required:"false"`
		Public      bool   `json:"public,omitempty" required:"false"`
	}
}

type PlaylistOutput struct {
	Body PlaylistDTO
}

func (s *Server) handleCreatePlaylist(ctx context.Context, in *CreatePlaylistInput) (*PlaylistOutput, error) {
	me := auth.FromContext(ctx)
	row, err := s.queries.CreatePlaylist(ctx, dbgen.CreatePlaylistParams{
		UserID: me.UserID, Name: in.Body.Name,
		Description: in.Body.Description, Public: in.Body.Public,
	})
	if err != nil {
		s.deps.Log.Error("create playlist failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not create playlist")
	}
	return &PlaylistOutput{Body: PlaylistDTO{
		ID: row.ID.String(), Name: row.Name, Description: row.Description,
		Public: row.Public, Owned: true, OwnerName: me.Email, CreatedAt: row.CreatedAt,
	}}, nil
}

type PlaylistIDInput struct {
	ID string `path:"id" format:"uuid"`
}

type GetPlaylistOutput struct {
	Body struct {
		Playlist PlaylistDTO        `json:"playlist"`
		Tracks   []PlaylistEntryDTO `json:"tracks"`
	}
}

func (s *Server) handleGetPlaylist(ctx context.Context, in *PlaylistIDInput) (*GetPlaylistOutput, error) {
	me := auth.FromContext(ctx)
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed playlist id")
	}

	// The query is scoped to "mine or public", so a playlist belonging to
	// somebody else is indistinguishable from one that does not exist.
	p, err := s.queries.GetPlaylist(ctx, dbgen.GetPlaylistParams{ID: id, UserID: me.UserID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("No such playlist")
		}
		s.deps.Log.Error("get playlist failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not load playlist")
	}

	entries, err := s.queries.ListPlaylistTracks(ctx, dbgen.ListPlaylistTracksParams{
		PlaylistID: id, UserID: me.UserID,
	})
	if err != nil {
		s.deps.Log.Error("list playlist tracks failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not load playlist tracks")
	}

	out := &GetPlaylistOutput{}
	out.Body.Playlist = PlaylistDTO{
		ID: p.ID.String(), Name: p.Name, Description: p.Description,
		Public: p.Public, Owned: p.Owned, OwnerName: p.OwnerName,
		TrackCount: p.TrackCount, DurationMs: p.DurationMs, CreatedAt: p.CreatedAt,
	}
	out.Body.Tracks = make([]PlaylistEntryDTO, 0, len(entries))
	for _, e := range entries {
		dto := trackRow{
			ID: e.ID, Title: e.Title, ArtistID: e.ArtistID, ArtistName: e.ArtistName,
			AlbumID: e.AlbumID, AlbumName: e.AlbumName, AlbumArtistName: e.AlbumArtistName,
			Genres: e.Genres, Year: e.Year, TrackNo: e.TrackNo, DiscNo: e.DiscNo,
			DurationMs: e.DurationMs, Bitrate: e.Bitrate, Suffix: e.Suffix,
			CoverArtID: e.CoverArtID,
		}.dto(false)
		dto.Favorite = e.Favorite
		out.Body.Tracks = append(out.Body.Tracks, PlaylistEntryDTO{
			EntryID: e.EntryID.String(), Track: dto, Unavailable: e.Unavailable,
		})
	}
	return out, nil
}

type UpdatePlaylistInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name        *string `json:"name,omitempty" minLength:"1" maxLength:"200"`
		Description *string `json:"description,omitempty" maxLength:"2000"`
		Public      *bool   `json:"public,omitempty"`
	}
}

func (s *Server) handleUpdatePlaylist(ctx context.Context, in *UpdatePlaylistInput) (*PlaylistOutput, error) {
	me := auth.FromContext(ctx)
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed playlist id")
	}

	row, err := s.queries.UpdatePlaylist(ctx, dbgen.UpdatePlaylistParams{
		ID: id, UserID: me.UserID,
		Name: in.Body.Name, Description: in.Body.Description, Public: in.Body.Public,
	})
	if err != nil {
		// The statement is scoped by user_id, so no row means either "missing"
		// or "not yours" — deliberately the same answer.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("No such playlist")
		}
		s.deps.Log.Error("update playlist failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not update playlist")
	}
	return &PlaylistOutput{Body: PlaylistDTO{
		ID: row.ID.String(), Name: row.Name, Description: row.Description,
		Public: row.Public, Owned: true, OwnerName: me.Email, CreatedAt: row.CreatedAt,
	}}, nil
}

func (s *Server) handleDeletePlaylist(ctx context.Context, in *PlaylistIDInput) (*NoContentOutput, error) {
	me := auth.FromContext(ctx)
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed playlist id")
	}
	n, err := s.queries.DeletePlaylist(ctx, dbgen.DeletePlaylistParams{ID: id, UserID: me.UserID})
	if err != nil {
		s.deps.Log.Error("delete playlist failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not delete playlist")
	}
	if n == 0 {
		return nil, huma.Error404NotFound("No such playlist")
	}
	return &NoContentOutput{Status: http.StatusNoContent}, nil
}

// ---- playlist contents ------------------------------------------------------------

type AddPlaylistTrackInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		TrackID string `json:"trackId" format:"uuid"`
		// A pointer so absent means "not confirmed". Adding a track already in
		// the playlist is legitimate — a set can repeat a song deliberately —
		// but it should be a decision, not the silent result of a mis-click.
		AllowDuplicate *bool `json:"allowDuplicate,omitempty"`
	}
}

func (s *Server) handleAddPlaylistTrack(ctx context.Context, in *AddPlaylistTrackInput) (*PlaylistOutput, error) {
	me := auth.FromContext(ctx)
	playlistID, trackID, err := parsePair(in.ID, in.Body.TrackID)
	if err != nil {
		return nil, err
	}
	if err := s.requireOwnedPlaylist(ctx, playlistID, me.UserID); err != nil {
		return nil, err
	}

	// Checked server-side rather than left to the client: any caller gets the
	// same protection, and there is no window between "client checked" and
	// "server appended" for a concurrent add to slip through unnoticed.
	if in.Body.AllowDuplicate == nil || !*in.Body.AllowDuplicate {
		duplicate, err := s.queries.PlaylistContainsTrack(ctx, dbgen.PlaylistContainsTrackParams{
			PlaylistID: playlistID, TrackID: trackID,
		})
		if err != nil {
			s.deps.Log.Error("duplicate check failed", "error", err)
			return nil, huma.Error500InternalServerError("Could not add track")
		}
		if duplicate {
			return nil, huma.Error409Conflict(
				"That track is already in this playlist. Send allowDuplicate to add it again.")
		}
	}

	if _, err := s.queries.AppendPlaylistTrack(ctx, dbgen.AppendPlaylistTrackParams{
		PlaylistID: playlistID, TrackID: trackID,
	}); err != nil {
		s.deps.Log.Error("append playlist track failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not add track")
	}
	return s.playlistSummary(ctx, playlistID, me)
}

type RemovePlaylistTrackInput struct {
	ID      string `path:"id" format:"uuid"`
	EntryID string `path:"entryId" format:"uuid"`
}

func (s *Server) handleRemovePlaylistTrack(ctx context.Context, in *RemovePlaylistTrackInput) (*NoContentOutput, error) {
	me := auth.FromContext(ctx)
	playlistID, entryID, err := parsePair(in.ID, in.EntryID)
	if err != nil {
		return nil, err
	}
	if err := s.requireOwnedPlaylist(ctx, playlistID, me.UserID); err != nil {
		return nil, err
	}

	n, err := s.queries.RemovePlaylistTrack(ctx, dbgen.RemovePlaylistTrackParams{
		ID: entryID, PlaylistID: playlistID,
	})
	if err != nil {
		s.deps.Log.Error("remove playlist track failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not remove track")
	}
	if n == 0 {
		return nil, huma.Error404NotFound("No such entry in this playlist")
	}
	// Positions are renumbered so they stay dense and the next append lands at
	// the end rather than in a hole.
	if err := s.queries.CompactPlaylistPositions(ctx, playlistID); err != nil {
		s.deps.Log.Warn("compact playlist positions failed", "error", err)
	}
	return &NoContentOutput{Status: http.StatusNoContent}, nil
}

type ReorderPlaylistInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		EntryIDs []string `json:"entryIds" doc:"Every entry id, in the desired order"`
	}
}

func (s *Server) handleReorderPlaylist(ctx context.Context, in *ReorderPlaylistInput) (*GetPlaylistOutput, error) {
	me := auth.FromContext(ctx)
	playlistID, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed playlist id")
	}
	if err := s.requireOwnedPlaylist(ctx, playlistID, me.UserID); err != nil {
		return nil, err
	}

	ids := make([]uuid.UUID, 0, len(in.Body.EntryIDs))
	for _, raw := range in.Body.EntryIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("Malformed entry id " + raw)
		}
		ids = append(ids, id)
	}

	// A partial list would leave the omitted entries at stale positions,
	// colliding with the new ones.
	count, err := s.queries.CountPlaylistTracks(ctx, playlistID)
	if err != nil {
		return nil, huma.Error500InternalServerError("Could not reorder playlist")
	}
	if int64(len(ids)) != count {
		return nil, huma.Error422UnprocessableEntity(
			"Reorder must list every entry in the playlist")
	}

	if err := s.queries.ReorderPlaylist(ctx, dbgen.ReorderPlaylistParams{
		EntryIds: ids, PlaylistID: playlistID,
	}); err != nil {
		s.deps.Log.Error("reorder playlist failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not reorder playlist")
	}
	return s.handleGetPlaylist(ctx, &PlaylistIDInput{ID: in.ID})
}

func (s *Server) requireOwnedPlaylist(ctx context.Context, playlistID, userID uuid.UUID) error {
	owned, err := s.queries.PlaylistIsOwnedBy(ctx, dbgen.PlaylistIsOwnedByParams{
		ID: playlistID, UserID: userID,
	})
	if err != nil {
		s.deps.Log.Error("playlist ownership check failed", "error", err)
		return huma.Error500InternalServerError("Could not verify playlist ownership")
	}
	if !owned {
		// 404 rather than 403: a public playlist is readable by everyone, but
		// whether one exists that you cannot edit is not worth confirming.
		return huma.Error404NotFound("No such playlist")
	}
	return nil
}

func (s *Server) playlistSummary(ctx context.Context, id uuid.UUID, me *auth.Identity) (*PlaylistOutput, error) {
	p, err := s.queries.GetPlaylist(ctx, dbgen.GetPlaylistParams{ID: id, UserID: me.UserID})
	if err != nil {
		return nil, huma.Error500InternalServerError("Could not load playlist")
	}
	return &PlaylistOutput{Body: PlaylistDTO{
		ID: p.ID.String(), Name: p.Name, Description: p.Description,
		Public: p.Public, Owned: p.Owned, OwnerName: p.OwnerName,
		TrackCount: p.TrackCount, DurationMs: p.DurationMs, CreatedAt: p.CreatedAt,
	}}, nil
}

func parsePair(a, b string) (uuid.UUID, uuid.UUID, error) {
	first, err := uuid.Parse(a)
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, huma.Error422UnprocessableEntity("Malformed id")
	}
	second, err := uuid.Parse(b)
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, huma.Error422UnprocessableEntity("Malformed id")
	}
	return first, second, nil
}

// ---- favourites ---------------------------------------------------------------------

type FavoriteInput struct {
	Type string `path:"type" enum:"track,album,artist"`
	ID   string `path:"id" format:"uuid"`
}

func (s *Server) handleAddFavorite(ctx context.Context, in *FavoriteInput) (*NoContentOutput, error) {
	me := auth.FromContext(ctx)
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed id")
	}
	if err := s.queries.AddFavorite(ctx, dbgen.AddFavoriteParams{
		UserID: me.UserID, EntityType: dbgen.FavoriteEntity(in.Type), EntityID: id,
	}); err != nil {
		s.deps.Log.Error("add favorite failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not save favourite")
	}
	return &NoContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) handleRemoveFavorite(ctx context.Context, in *FavoriteInput) (*NoContentOutput, error) {
	me := auth.FromContext(ctx)
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed id")
	}
	if _, err := s.queries.RemoveFavorite(ctx, dbgen.RemoveFavoriteParams{
		UserID: me.UserID, EntityType: dbgen.FavoriteEntity(in.Type), EntityID: id,
	}); err != nil {
		s.deps.Log.Error("remove favorite failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not remove favourite")
	}
	// Idempotent: unfavouriting something that was not favourited is a success,
	// which keeps a double-click from producing an error.
	return &NoContentOutput{Status: http.StatusNoContent}, nil
}

type ListFavoritesInput struct {
	Limit int `query:"limit" default:"200" minimum:"1" maximum:"500"`
}

func (s *Server) handleListFavorites(ctx context.Context, in *ListFavoritesInput) (*ListTracksOutput, error) {
	me := auth.FromContext(ctx)
	rows, err := s.queries.ListFavoriteTracks(ctx, dbgen.ListFavoriteTracksParams{
		UserID: me.UserID, Limit: int32(clampLimit(in.Limit)),
	})
	if err != nil {
		s.deps.Log.Error("list favorites failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not list favourites")
	}
	out := &ListTracksOutput{}
	out.Body.Tracks = make([]TrackDTO, 0, len(rows))
	for _, r := range rows {
		dto := trackRow{
			ID: r.ID, Title: r.Title, ArtistID: r.ArtistID, ArtistName: r.ArtistName,
			AlbumID: r.AlbumID, AlbumName: r.AlbumName, AlbumArtistName: r.AlbumArtistName,
			Genres: r.Genres, Year: r.Year, TrackNo: r.TrackNo, DiscNo: r.DiscNo,
			DurationMs: r.DurationMs, Bitrate: r.Bitrate, Suffix: r.Suffix,
			CoverArtID: r.CoverArtID,
		}.dto(false)
		dto.Favorite = true
		out.Body.Tracks = append(out.Body.Tracks, dto)
	}
	return out, nil
}

// ---- plays -----------------------------------------------------------------------------

type RecordPlayInput struct {
	Body struct {
		TrackID  string `json:"trackId" format:"uuid"`
		MsPlayed int    `json:"msPlayed" minimum:"0" doc:"Milliseconds actually listened to"`
		// A pointer, not a string with a default: huma marks a plain field
		// required even with default and required:"false" set, which forces
		// every client to send it.
		Source *string `json:"source,omitempty" enum:"library,youtube"`
	}
}

type RecordPlayOutput struct {
	Body struct {
		Counted bool  `json:"counted" doc:"False when the play was too short to qualify"`
		Needed  int64 `json:"neededMs" doc:"Milliseconds required for this track to count"`
	}
}

func (s *Server) handleRecordPlay(ctx context.Context, in *RecordPlayInput) (*RecordPlayOutput, error) {
	me := auth.FromContext(ctx)
	trackID, err := uuid.Parse(in.Body.TrackID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed track id")
	}

	// The threshold is checked against the duration the server knows, not one
	// the client reports: otherwise anyone could inflate their own counts by
	// claiming a play a moment after pressing play.
	duration, err := s.queries.GetTrackDuration(ctx, trackID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("No such track")
		}
		s.deps.Log.Error("load track duration failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not record play")
	}

	needed := requiredListenMs(duration)
	out := &RecordPlayOutput{}
	out.Body.Needed = needed

	if int64(in.Body.MsPlayed) < needed {
		out.Body.Counted = false
		return out, nil
	}

	source := "library"
	if in.Body.Source != nil && *in.Body.Source != "" {
		source = *in.Body.Source
	}
	if _, err := s.queries.RecordPlay(ctx, dbgen.RecordPlayParams{
		UserID: me.UserID, TrackID: pgtype.UUID{Bytes: trackID, Valid: true},
		MsPlayed: int32(in.Body.MsPlayed), Source: source,
	}); err != nil {
		s.deps.Log.Error("record play failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not record play")
	}
	out.Body.Counted = true
	return out, nil
}

// requiredListenMs is Last.fm's rule: half the track, or four minutes,
// whichever comes first. A track with no known duration falls back to the
// ceiling rather than counting instantly.
func requiredListenMs(durationMs *int32) int64 {
	ceiling := scrobbleCeiling.Milliseconds()
	if durationMs == nil || *durationMs <= 0 {
		return ceiling
	}
	half := int64(float64(*durationMs) * scrobbleFraction)
	return min(half, ceiling)
}

type HistoryInput struct {
	Limit int `query:"limit" default:"50" minimum:"1" maximum:"200"`
}

type HistoryEntryDTO struct {
	PlayedAt   time.Time `json:"playedAt"`
	MsPlayed   int32     `json:"msPlayed"`
	Source     string    `json:"source"`
	TrackID    string    `json:"trackId,omitempty" required:"false"`
	Title      string    `json:"title"`
	ArtistName string    `json:"artistName"`
	AlbumName  string    `json:"albumName"`
	CoverArtID string    `json:"coverArtId,omitempty" required:"false"`
}

type HistoryOutput struct {
	Body struct {
		History []HistoryEntryDTO `json:"history"`
	}
}

func (s *Server) handleListHistory(ctx context.Context, in *HistoryInput) (*HistoryOutput, error) {
	me := auth.FromContext(ctx)
	rows, err := s.queries.ListHistory(ctx, dbgen.ListHistoryParams{
		UserID: me.UserID, Limit: int32(clampLimit(in.Limit)),
	})
	if err != nil {
		s.deps.Log.Error("list history failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not load history")
	}

	out := &HistoryOutput{}
	out.Body.History = make([]HistoryEntryDTO, 0, len(rows))
	for _, r := range rows {
		entry := HistoryEntryDTO{
			PlayedAt: r.PlayedAt, MsPlayed: r.MsPlayed, Source: r.Source,
		}
		// A play whose track was later deleted keeps its row; the listing shows
		// it as an unknown track rather than dropping the history.
		if r.TrackID.Valid {
			entry.TrackID = uuidString(r.TrackID)
			entry.Title = derefString(r.Title)
			entry.ArtistName = derefString(r.ArtistName)
			entry.AlbumName = derefString(r.AlbumName)
			entry.CoverArtID = uuidString(r.CoverArtID)
		} else {
			entry.Title = "(removed from library)"
		}
		out.Body.History = append(out.Body.History, entry)
	}
	return out, nil
}

func (s *Server) handleTopTracks(ctx context.Context, in *HistoryInput) (*ListTracksOutput, error) {
	me := auth.FromContext(ctx)
	rows, err := s.queries.TopTracks(ctx, dbgen.TopTracksParams{
		UserID: me.UserID, Limit: int32(clampLimit(in.Limit)),
	})
	if err != nil {
		s.deps.Log.Error("top tracks failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not load top tracks")
	}
	out := &ListTracksOutput{}
	out.Body.Tracks = make([]TrackDTO, 0, len(rows))
	for _, r := range rows {
		out.Body.Tracks = append(out.Body.Tracks, trackRow{
			ID: r.ID, Title: r.Title, ArtistID: r.ArtistID, ArtistName: r.ArtistName,
			AlbumID: r.AlbumID, AlbumName: r.AlbumName, AlbumArtistName: r.AlbumArtistName,
			Genres: r.Genres, Year: r.Year, TrackNo: r.TrackNo, DiscNo: r.DiscNo,
			DurationMs: r.DurationMs, Bitrate: r.Bitrate, Suffix: r.Suffix,
			CoverArtID: r.CoverArtID,
		}.dto(false))
	}
	return out, nil
}

// derefString flattens the nullable columns a LEFT JOIN produces.
func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
