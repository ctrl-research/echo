package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/jonathanng/echo/internal/db/dbgen"
	"github.com/jonathanng/echo/internal/youtube"
)

type YTItemDTO struct {
	VideoID    string     `json:"videoId"`
	Title      string     `json:"title"`
	Uploader   string     `json:"uploader"`
	DurationMs int32      `json:"durationMs,omitempty" required:"false"`
	Thumbnail  string     `json:"thumbnailUrl,omitempty" required:"false"`
	State      string     `json:"state" enum:"pending,downloading,ready,failed,evicted"`
	Error      string     `json:"error,omitempty" required:"false"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty" required:"false" doc:"When the cached copy is eligible for eviction"`
	Promoted   bool       `json:"promoted" doc:"Copied into the library, and no longer subject to eviction"`
	TrackID    string     `json:"trackId,omitempty" required:"false"`
}

func ytItemDTO(item dbgen.YtItem) YTItemDTO {
	dto := YTItemDTO{
		VideoID: item.VideoID, Title: item.Title, Uploader: item.Uploader,
		State: string(item.State), Promoted: item.PromotedTrackID.Valid,
	}
	if item.DurationMs != nil {
		dto.DurationMs = *item.DurationMs
	}
	if item.ThumbnailUrl != nil {
		dto.Thumbnail = *item.ThumbnailUrl
	}
	if item.Error != nil {
		dto.Error = *item.Error
	}
	if item.ExpiresAt.Valid {
		t := item.ExpiresAt.Time
		dto.ExpiresAt = &t
	}
	if item.PromotedTrackID.Valid {
		dto.TrackID = uuidString(item.PromotedTrackID)
	}
	return dto
}

func (s *Server) registerYouTube() {
	reg := func(id, method, path, summary string, status int) huma.Operation {
		return huma.Operation{
			OperationID: id, Method: method, Path: path,
			Summary: summary, DefaultStatus: status, Tags: []string{"youtube"},
		}
	}

	huma.Register(s.API, reg("youtubeStatus", http.MethodGet, "/youtube",
		"Whether YouTube support is available", http.StatusOK), s.handleYTStatus)
	huma.Register(s.API, reg("youtubeSearch", http.MethodGet, "/youtube/search",
		"Search YouTube", http.StatusOK), s.handleYTSearch)
	huma.Register(s.API, reg("youtubePrepare", http.MethodPost, "/youtube/prepare",
		"Queue a video for playback", http.StatusAccepted), s.handleYTPrepare)
	huma.Register(s.API, reg("youtubeItem", http.MethodGet, "/youtube/{videoId}",
		"Cache status for one video", http.StatusOK), s.handleYTItem)
	huma.Register(s.API, reg("youtubePromote", http.MethodPost, "/youtube/{videoId}/promote",
		"Copy a cached video into the library", http.StatusAccepted), s.handleYTPromote)
}

// registerYouTubeMedia mounts the audio route on chi, for the same reason the
// library's stream route is there: ServeContent needs the raw writer.
func (s *Server) registerYouTubeMedia(apiRouter chi.Router) {
	apiRouter.Get("/youtube/{videoId}/stream", s.handleYTStream)
}

// ---- status and search --------------------------------------------------------

type YTStatusOutput struct {
	Body struct {
		Available bool   `json:"available" doc:"False when yt-dlp is not installed"`
		Version   string `json:"version,omitempty" required:"false"`
	}
}

func (s *Server) handleYTStatus(ctx context.Context, _ *struct{}) (*YTStatusOutput, error) {
	out := &YTStatusOutput{}
	if s.deps.YouTube != nil {
		out.Body.Available = s.deps.YouTube.Available()
		out.Body.Version = s.deps.YouTube.Version(ctx)
	}
	return out, nil
}

type YTSearchInput struct {
	Query string `query:"q" minLength:"1" maxLength:"200"`
	Limit int    `query:"limit" default:"20" minimum:"1" maximum:"50"`
}

type YTSearchOutput struct {
	Body struct {
		Results []YTResultDTO `json:"results"`
	}
}

type YTResultDTO struct {
	VideoID    string `json:"videoId"`
	Title      string `json:"title"`
	Uploader   string `json:"uploader"`
	DurationMs int64  `json:"durationMs,omitempty" required:"false"`
	Thumbnail  string `json:"thumbnailUrl,omitempty" required:"false"`
	// Cached says the audio is already held locally, so playback is immediate.
	Cached bool `json:"cached"`
}

func (s *Server) handleYTSearch(ctx context.Context, in *YTSearchInput) (*YTSearchOutput, error) {
	if err := s.ytReady(); err != nil {
		return nil, err
	}

	results, err := s.deps.YouTube.Search(ctx, in.Query, in.Limit)
	if err != nil {
		s.deps.Log.Error("youtube search failed", "error", err)
		return nil, huma.Error502BadGateway(
			"YouTube search failed. This usually means yt-dlp needs updating.")
	}

	out := &YTSearchOutput{}
	out.Body.Results = make([]YTResultDTO, 0, len(results))
	for _, r := range results {
		dto := YTResultDTO{
			VideoID: r.VideoID, Title: r.Title, Uploader: r.Uploader,
			DurationMs: r.DurationMs, Thumbnail: r.ThumbnailURL,
		}
		// Cheap per-result lookup so the UI can distinguish "plays instantly"
		// from "needs a download first".
		if item, err := s.deps.YouTube.Get(ctx, r.VideoID); err == nil {
			dto.Cached = item.State == dbgen.YtStateReady
		}
		out.Body.Results = append(out.Body.Results, dto)
	}
	return out, nil
}

// ---- preparation and status ------------------------------------------------------

type YTPrepareInput struct {
	Body struct {
		VideoID string `json:"videoId" minLength:"5" maxLength:"32"`
		// Metadata from the search result, so an item has something to display
		// while it downloads rather than appearing as a bare id.
		Title      string `json:"title,omitempty" maxLength:"500" required:"false"`
		Uploader   string `json:"uploader,omitempty" maxLength:"200" required:"false"`
		DurationMs int64  `json:"durationMs,omitempty" required:"false"`
		Thumbnail  string `json:"thumbnailUrl,omitempty" maxLength:"1000" required:"false"`
	}
}

type YTItemOutput struct {
	Body YTItemDTO
}

func (s *Server) handleYTPrepare(ctx context.Context, in *YTPrepareInput) (*YTItemOutput, error) {
	if err := s.ytReady(); err != nil {
		return nil, err
	}

	item, err := s.deps.YouTube.Prepare(ctx, youtube.SearchResult{
		VideoID: in.Body.VideoID, Title: in.Body.Title, Uploader: in.Body.Uploader,
		DurationMs: in.Body.DurationMs, ThumbnailURL: in.Body.Thumbnail,
	})
	if err != nil {
		s.deps.Log.Error("youtube prepare failed", "video", in.Body.VideoID, "error", err)
		return nil, huma.Error500InternalServerError("Could not queue this video")
	}
	return &YTItemOutput{Body: ytItemDTO(item)}, nil
}

type YTVideoInput struct {
	VideoID string `path:"videoId" maxLength:"32"`
}

func (s *Server) handleYTItem(ctx context.Context, in *YTVideoInput) (*YTItemOutput, error) {
	if err := s.ytReady(); err != nil {
		return nil, err
	}
	item, err := s.deps.YouTube.Get(ctx, in.VideoID)
	if err != nil {
		if errors.Is(err, youtube.ErrNotFound) {
			return nil, huma.Error404NotFound("This instance knows nothing about that video")
		}
		return nil, huma.Error500InternalServerError("Could not load item")
	}
	return &YTItemOutput{Body: ytItemDTO(item)}, nil
}

// ---- promotion ----------------------------------------------------------------------

type YTPromoteOutput struct {
	Body struct {
		Path string `json:"path" doc:"Where the file was written under the library root"`
	}
}

func (s *Server) handleYTPromote(ctx context.Context, in *YTVideoInput) (*YTPromoteOutput, error) {
	if err := s.ytReady(); err != nil {
		return nil, err
	}

	path, err := s.deps.YouTube.Promote(ctx, in.VideoID)
	switch {
	case errors.Is(err, youtube.ErrNotFound):
		return nil, huma.Error404NotFound("This instance knows nothing about that video")
	case errors.Is(err, youtube.ErrNotCached):
		return nil, huma.Error409Conflict(
			"That video is not cached yet. Play it first, then promote it.")
	case err != nil:
		s.deps.Log.Error("youtube promote failed", "video", in.VideoID, "error", err)
		return nil, huma.Error500InternalServerError("Could not promote this video")
	}

	// The watcher will pick the new file up and scan it; the cache entry stays
	// valid until then, so playback is uninterrupted.
	out := &YTPromoteOutput{}
	out.Body.Path = path
	return out, nil
}

// ---- streaming ------------------------------------------------------------------------

func (s *Server) handleYTStream(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	if s.deps.YouTube == nil {
		apiError(http.StatusServiceUnavailable, "Service Unavailable",
			"YouTube support is not configured.")(w, r)
		return
	}

	videoID := chi.URLParam(r, "videoId")
	key, err := s.deps.YouTube.BlobKey(r.Context(), videoID)
	switch {
	case errors.Is(err, youtube.ErrNotFound):
		apiError(http.StatusNotFound, "Not Found",
			"This instance knows nothing about that video.")(w, r)
		return
	case errors.Is(err, youtube.ErrNotCached):
		// Not an error the client should retry blindly: it means the download
		// has not finished, and polling the item endpoint says when it has.
		apiError(http.StatusConflict, "Conflict",
			"That video is still downloading. Poll the item endpoint for its state.")(w, r)
		return
	case err != nil:
		s.deps.Log.Error("youtube stream failed", "video", videoID, "error", err)
		apiError(http.StatusInternalServerError, "Internal Server Error",
			"Could not stream this video.")(w, r)
		return
	}

	if err := s.deps.Media.ServeBlobAudio(w, r, key); err != nil {
		s.deps.Log.Error("serve youtube audio failed", "video", videoID, "error", err)
		apiError(http.StatusInternalServerError, "Internal Server Error",
			"Could not stream this video.")(w, r)
	}
}

func (s *Server) ytReady() error {
	if s.deps.YouTube == nil {
		return huma.Error503ServiceUnavailable("YouTube support is not configured")
	}
	if !s.deps.YouTube.Available() {
		return huma.Error503ServiceUnavailable(
			"yt-dlp is not installed on the server, so YouTube is unavailable")
	}
	return nil
}
