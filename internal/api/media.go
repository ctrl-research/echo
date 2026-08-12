package api

import (
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jonathanng/echo/internal/media"
)

// Media routes are registered on chi rather than through huma.
//
// huma's typed handlers marshal a Go value into the response body, which is the
// wrong shape for audio: these endpoints need the raw http.ResponseWriter and
// *http.Request so http.ServeContent can do the range and conditional handling.
// They still sit on the API sub-router, so the session and CSRF middleware
// apply exactly as they do everywhere else.
//
// They are added to the OpenAPI document separately, in documentMediaRoutes, so
// the spec stays a complete description of the surface.
func (s *Server) registerMedia(apiRouter chi.Router) {
	apiRouter.Get("/tracks/{id}/stream", s.handleStreamTrack)
	apiRouter.Get("/art/{id}", s.handleCoverArt)
	apiRouter.Get("/albums/{id}/art", s.handleAlbumArt)
}

func (s *Server) handleStreamTrack(w http.ResponseWriter, r *http.Request) {
	// Registered on chi, so the huma guard chain does not run: the check has to
	// be made here explicitly.
	if !s.requireSession(w, r) {
		return
	}
	id, ok := s.parseIDParam(w, r)
	if !ok {
		return
	}

	err := s.deps.Media.ServeTrack(w, r, id)
	switch {
	case err == nil:
	case errors.Is(err, media.ErrNotFound):
		apiError(http.StatusNotFound, "Not Found", "No such track.")(w, r)
	case errors.Is(err, media.ErrUnsupported):
		apiError(http.StatusUnsupportedMediaType, "Unsupported Media Type",
			"This file needs transcoding, but ffmpeg is not available on the server.")(w, r)
	default:
		s.deps.Log.Error("stream track failed", "track", id, "error", err)
		// ServeContent may already have written a header, in which case this
		// is a no-op with a warning; there is nothing better to do once the
		// body has started.
		apiError(http.StatusInternalServerError, "Internal Server Error",
			"Could not stream this track.")(w, r)
	}
}

func (s *Server) handleCoverArt(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	id, ok := s.parseIDParam(w, r)
	if !ok {
		return
	}
	s.writeArt(w, r, s.deps.Media.ServeCoverArt(w, r, id))
}

func (s *Server) handleAlbumArt(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r) {
		return
	}
	id, ok := s.parseIDParam(w, r)
	if !ok {
		return
	}
	s.writeArt(w, r, s.deps.Media.ServeAlbumCoverArt(w, r, id))
}

func (s *Server) writeArt(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == nil:
	case errors.Is(err, media.ErrNotFound):
		apiError(http.StatusNotFound, "Not Found", "No artwork for this item.")(w, r)
	default:
		s.deps.Log.Error("serve cover art failed", "error", err)
		apiError(http.StatusInternalServerError, "Internal Server Error",
			"Could not load artwork.")(w, r)
	}
}

// requireSession enforces authentication for the chi-registered media routes,
// which bypass the huma guard chain.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) bool {
	if s.deps.Media == nil {
		apiError(http.StatusServiceUnavailable, "Service Unavailable",
			"Media serving is not configured.")(w, r)
		return false
	}
	if authIdentity(r) == nil {
		apiError(http.StatusUnauthorized, "Unauthorized",
			"Authentication is required for this endpoint.")(w, r)
		return false
	}
	return true
}

func (s *Server) parseIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		apiError(http.StatusUnprocessableEntity, "Unprocessable Entity",
			"Malformed identifier.")(w, r)
		return uuid.UUID{}, false
	}
	return id, true
}

// documentMediaRoutes adds the chi-served endpoints to the generated spec, so
// `echo openapi` describes the whole surface rather than only the huma half.
func documentMediaRoutes(api huma.API) {
	spec := api.OpenAPI()

	binary := &huma.Response{
		Description: "Audio stream. Supports Range requests.",
		Content: map[string]*huma.MediaType{
			"audio/*": {Schema: &huma.Schema{Type: "string", Format: "binary"}},
		},
	}
	image := &huma.Response{
		Description: "Image bytes.",
		Content: map[string]*huma.MediaType{
			"image/*": {Schema: &huma.Schema{Type: "string", Format: "binary"}},
		},
	}
	idParam := []*huma.Param{{
		Name: "id", In: "path", Required: true,
		Schema: &huma.Schema{Type: "string", Format: "uuid"},
	}}

	for path, op := range map[string]*huma.Operation{
		"/tracks/{id}/stream": {
			OperationID: "streamTrack",
			Summary:     "Stream a track's audio",
			Description: "Serves the original file, or a cached transcode when the " +
				"format is not natively playable. Honours Range requests.",
			Tags:       []string{"media"},
			Parameters: idParam,
			Responses: map[string]*huma.Response{
				"200": binary,
				"206": {Description: "Partial content, in response to a Range request."},
				"404": {Description: "No such track."},
				"415": {Description: "Needs transcoding, but ffmpeg is unavailable."},
			},
		},
		"/art/{id}": {
			OperationID: "coverArt", Summary: "Fetch cover art by id",
			Tags: []string{"media"}, Parameters: idParam,
			Responses: map[string]*huma.Response{"200": image, "404": {Description: "No artwork."}},
		},
		"/albums/{id}/art": {
			OperationID: "albumArt", Summary: "Fetch an album's cover art",
			Tags: []string{"media"}, Parameters: idParam,
			Responses: map[string]*huma.Response{"200": image, "404": {Description: "No artwork."}},
		},
	} {
		item := spec.Paths[path]
		if item == nil {
			item = &huma.PathItem{}
			spec.Paths[path] = item
		}
		item.Get = op
	}
}
