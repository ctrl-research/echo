package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jonathanng/echo/internal/db/dbgen"
)

func (s *Server) registerLibraryAdmin() {
	huma.Register(s.API, huma.Operation{
		OperationID: "libraryStats",
		Method:      http.MethodGet,
		Path:        "/admin/library",
		Summary:     "Library totals and root status",
		Tags:        []string{"admin"},
	}, s.handleLibraryStats)

	huma.Register(s.API, huma.Operation{
		OperationID:   "triggerScan",
		Method:        http.MethodPost,
		Path:          "/admin/library/scan",
		Summary:       "Queue a scan of every root",
		Description:   "Returns immediately; the scan runs as a background job.",
		DefaultStatus: http.StatusAccepted,
		Tags:          []string{"admin"},
	}, s.handleTriggerScan)

	huma.Register(s.API, huma.Operation{
		OperationID: "listJobs",
		Method:      http.MethodGet,
		Path:        "/admin/jobs",
		Summary:     "Recent background jobs",
		Tags:        []string{"admin"},
	}, s.handleListJobs)
}

// ---- stats -------------------------------------------------------------------

type RootDTO struct {
	ID        string     `json:"id" format:"uuid"`
	Path      string     `json:"path"`
	Writable  bool       `json:"writable"`
	ScannedAt *time.Time `json:"scannedAt,omitempty" required:"false"`
	Scanning  bool       `json:"scanning" doc:"A scan is in progress"`
	Error     string     `json:"error,omitempty" required:"false"`
}

type LibraryStatsOutput struct {
	Body struct {
		Tracks          int64     `json:"tracks"`
		Missing         int64     `json:"missing" doc:"Tracks whose file has disappeared"`
		Albums          int64     `json:"albums"`
		Artists         int64     `json:"artists"`
		Genres          int64     `json:"genres"`
		TotalDurationMs int64     `json:"totalDurationMs"`
		Roots           []RootDTO `json:"roots"`
	}
}

func (s *Server) handleLibraryStats(ctx context.Context, _ *struct{}) (*LibraryStatsOutput, error) {
	stats, err := s.deps.Library.Stats(ctx)
	if err != nil {
		s.deps.Log.Error("library stats failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not load library stats")
	}
	roots, err := s.queries.ListLibraryRoots(ctx)
	if err != nil {
		s.deps.Log.Error("list roots failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not load library roots")
	}

	out := &LibraryStatsOutput{}
	out.Body.Tracks = stats.Tracks
	out.Body.Missing = stats.Missing
	out.Body.Albums = stats.Albums
	out.Body.Artists = stats.Artists
	out.Body.Genres = stats.Genres
	out.Body.TotalDurationMs = toInt64(stats.TotalDurationMs)
	out.Body.Roots = make([]RootDTO, 0, len(roots))

	for _, r := range roots {
		dto := RootDTO{ID: r.ID.String(), Path: r.Path, Writable: r.Writable}
		if r.LastScanFinishedAt.Valid {
			t := r.LastScanFinishedAt.Time
			dto.ScannedAt = &t
		}
		// A start with no matching finish means a scan is running, or died.
		dto.Scanning = r.LastScanStartedAt.Valid &&
			(!r.LastScanFinishedAt.Valid || r.LastScanFinishedAt.Time.Before(r.LastScanStartedAt.Time))
		if r.LastScanError != nil {
			dto.Error = *r.LastScanError
		}
		out.Body.Roots = append(out.Body.Roots, dto)
	}
	return out, nil
}

// toInt64 flattens the numeric type sum(...) produces, which pgx surfaces as an
// interface because SUM over bigint is numeric.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

// ---- scan --------------------------------------------------------------------

type TriggerScanOutput struct {
	Body struct {
		Queued int `json:"queued" doc:"Number of roots queued for scanning"`
	}
}

func (s *Server) handleTriggerScan(ctx context.Context, _ *struct{}) (*TriggerScanOutput, error) {
	n, err := s.deps.Library.EnqueueScanAll(ctx)
	if err != nil {
		s.deps.Log.Error("enqueue scan failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not queue scan")
	}
	out := &TriggerScanOutput{}
	out.Body.Queued = n
	return out, nil
}

// ---- jobs --------------------------------------------------------------------

type JobDTO struct {
	ID         string     `json:"id" format:"uuid"`
	Type       string     `json:"type"`
	State      string     `json:"state" enum:"queued,running,done,failed"`
	Attempts   int32      `json:"attempts"`
	Error      string     `json:"error,omitempty" required:"false"`
	CreatedAt  time.Time  `json:"createdAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty" required:"false"`
}

type ListJobsInput struct {
	Limit int `query:"limit" default:"50" minimum:"1" maximum:"200"`
}

type ListJobsOutput struct {
	Body struct {
		Jobs   []JobDTO         `json:"jobs"`
		Counts map[string]int64 `json:"counts" doc:"Job count by state"`
	}
}

func (s *Server) handleListJobs(ctx context.Context, in *ListJobsInput) (*ListJobsOutput, error) {
	limit := int32(in.Limit)
	if limit <= 0 {
		limit = 50
	}

	rows, err := s.queries.ListJobs(ctx, dbgen.ListJobsParams{Limit: limit})
	if err != nil {
		s.deps.Log.Error("list jobs failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not list jobs")
	}
	counts, err := s.queries.CountJobsByState(ctx)
	if err != nil {
		s.deps.Log.Error("count jobs failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not count jobs")
	}

	out := &ListJobsOutput{}
	out.Body.Jobs = make([]JobDTO, 0, len(rows))
	for _, j := range rows {
		dto := JobDTO{
			ID: j.ID.String(), Type: j.Type, State: string(j.State),
			Attempts: j.Attempts, CreatedAt: j.CreatedAt,
		}
		if j.Error != nil {
			dto.Error = *j.Error
		}
		if j.FinishedAt.Valid {
			t := j.FinishedAt.Time
			dto.FinishedAt = &t
		}
		out.Body.Jobs = append(out.Body.Jobs, dto)
	}

	out.Body.Counts = make(map[string]int64, len(counts))
	for _, c := range counts {
		out.Body.Counts[string(c.State)] = c.Count
	}
	return out, nil
}
