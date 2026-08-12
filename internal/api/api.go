// Package api wires the HTTP surface: a chi router, huma for typed handlers
// and OpenAPI generation, and the embedded web client.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jonathanng/echo/internal/auth"
	"github.com/jonathanng/echo/internal/db/dbgen"
	"github.com/jonathanng/echo/internal/library"
	"github.com/jonathanng/echo/internal/version"
)

// APIPrefix is where the JSON API is mounted. The generated client uses the
// same value as its baseUrl, via the OpenAPI document's server entry.
const APIPrefix = "/api/v1"

// Deps are the collaborators the HTTP layer needs. Handlers depend on this
// struct rather than on package-level state so tests can supply their own.
type Deps struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// Auth owns sign-in, sessions, and provider discovery. Nil disables the
	// authenticated surface, which is what `echo openapi` and the routing
	// tests construct.
	Auth *auth.Service
	// Library owns scanning and library stats. Nil disables the library
	// endpoints, which is what `echo openapi` and the routing tests construct.
	Library *library.Service
	// WebFS serves the built client. Nil disables static serving, which is
	// what tests and `go run` without a client build want.
	WebFS http.FileSystem
}

// Server bundles the router and the huma API description.
type Server struct {
	Router  chi.Router
	API     huma.API
	deps    Deps
	queries *dbgen.Queries
}

// New builds the full HTTP surface.
func New(deps Deps) *Server {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))
	// Route HEAD to the GET handler when no HEAD route is registered. chi does
	// not do this by default, so HEAD would answer 405. Audio players issue
	// HEAD against media URLs to read Content-Length and Accept-Ranges before
	// they start ranged reads, and http.ServeContent answers those correctly
	// once the request reaches it.
	r.Use(middleware.GetHead)
	// On the root router, not the API sub-router: the OAuth callback lives at
	// the site root and creates a session, so it needs the caller address too.
	r.Use(auth.ClientIPMiddleware)

	cfg := huma.DefaultConfig("Echo", version.String())
	cfg.Info.Description = "Self-hosted music server: local library, YouTube streaming, offline playback."
	// Documentation only — huma does not route on this. Operation paths stay
	// version-free ("/health"), and the prefix below does the actual routing,
	// so the generated client's baseUrl and the server agree.
	cfg.Servers = []*huma.Server{{URL: APIPrefix}}

	// The API lives on its own sub-router mounted under the version prefix.
	// Registering huma directly on the root router would serve operations at
	// "/health" while advertising "/api/v1/health".
	apiRouter := chi.NewRouter()

	s := &Server{Router: r, deps: deps}
	if deps.Pool != nil {
		s.queries = dbgen.New(deps.Pool)
	}

	// chi panics if Use is called after any route is registered, so the whole
	// middleware stack has to be installed before huma registers a single
	// operation. Without an auth service there are no sessions to resolve, so
	// the auth stack is skipped rather than run against a nil service — the
	// routing tests construct exactly that.
	if s.deps.Auth != nil {
		apiRouter.Use(s.sessionMiddleware)
		apiRouter.Use(s.csrfMiddleware)
	}

	// chi propagates the parent's NotFound handler into mounted sub-routers,
	// which would make an unknown API path fall through to the SPA shell and
	// answer 200 with HTML. API clients must get a JSON error instead.
	apiRouter.NotFound(apiError(http.StatusNotFound, "Not Found",
		"No API operation matches this path."))
	apiRouter.MethodNotAllowed(apiError(http.StatusMethodNotAllowed, "Method Not Allowed",
		"This method is not supported for this path."))

	s.API = humachi.New(apiRouter, cfg)

	// Every operation is registered unconditionally, including when there is no
	// database. `echo openapi` builds a Server with no pool to render the spec,
	// and gating registration on the pool would silently drop endpoints from
	// the document — and therefore from the generated client.
	s.applyRouteGuards()
	s.registerHealth()
	s.registerAuth()
	s.registerUsers()
	s.registerLibraryAdmin()
	s.registerBrowse()

	r.Mount(APIPrefix, apiRouter)

	// Browser-facing OAuth redirects live at the site root, not under the API
	// prefix: the identity provider redirects the user's browser here, and the
	// response is a 302 rather than JSON. Registered before the SPA fallback so
	// they win over it.
	if s.deps.Auth != nil {
		s.deps.Auth.RegisterRoutes(r)
	}

	if deps.WebFS != nil {
		s.registerWeb()
	}
	return s
}

// applyRouteGuards attaches authorisation to operations after registration.
//
// huma registers handlers on the router as it goes, so guards are applied here
// as chi middleware scoped by path prefix. Keeping them in one place means the
// protected surface can be read at a glance instead of being inferred from
// per-handler checks.
func (s *Server) applyRouteGuards() {
	s.API.UseMiddleware(func(hctx huma.Context, next func(huma.Context)) {
		path := hctx.Operation().Path

		// Operations are registered even without a database so the spec stays
		// complete; everything except health needs one to actually run.
		if (s.queries == nil || s.deps.Auth == nil) && path != "/health" {
			_ = huma.WriteErr(s.API, hctx, http.StatusServiceUnavailable,
				"The database is not configured.")
			return
		}
		if s.deps.Library == nil && strings.HasPrefix(path, "/admin/library") {
			_ = huma.WriteErr(s.API, hctx, http.StatusServiceUnavailable,
				"The library service is not configured.")
			return
		}

		// Default deny. Authorisation is decided by an explicit allowlist of
		// public paths rather than by naming the protected ones, so an endpoint
		// added later is private until somebody deliberately opens it. The
		// inverse — listing what to protect — makes the failure mode "new
		// endpoint silently serves the whole library to anonymous callers".
		switch {
		case publicPaths[path]:
			// No session required; the client calls these before it has one.
		case strings.HasPrefix(path, "/admin"):
			if !s.guard(hctx, true) {
				return
			}
		default:
			if !s.guard(hctx, false) {
				return
			}
		}
		next(hctx)
	})
}

// publicPaths may be called without a session. Everything else requires one.
//
// Keep this list short and justify each entry: health is what probes call,
// providers is how the sign-in page knows which buttons to show, and login is
// how a session is obtained in the first place.
var publicPaths = map[string]bool{
	"/health":         true,
	"/auth/providers": true,
	"/auth/login":     true,
}

// guard writes an error and reports false when the caller is not permitted.
func (s *Server) guard(hctx huma.Context, adminOnly bool) bool {
	identity := auth.FromContext(hctx.Context())
	if identity == nil {
		// /auth/me answers 401 itself with a friendlier message; both are 401,
		// so the distinction is cosmetic.
		_ = huma.WriteErr(s.API, hctx, http.StatusUnauthorized,
			"Authentication is required for this endpoint.")
		return false
	}
	if adminOnly && !identity.IsAdmin() {
		_ = huma.WriteErr(s.API, hctx, http.StatusForbidden,
			"This endpoint requires the admin role.")
		return false
	}
	return true
}

// OpenAPI renders the generated specification. Used by `echo openapi`, which
// feeds openapi-typescript so the client's types are never hand-maintained.
func (s *Server) OpenAPI() ([]byte, error) {
	return s.API.OpenAPI().YAML()
}

// apiError renders a problem document matching huma's error shape, so that
// router-level failures are indistinguishable from handler-level ones to a
// client.
func apiError(status int, title, detail string) http.HandlerFunc {
	body, err := json.Marshal(map[string]any{
		"title": title, "status": status, "detail": detail,
	})
	if err != nil {
		panic("api: marshal static error body: " + err.Error())
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// ---- health ----------------------------------------------------------------

type HealthOutput struct {
	Body struct {
		Status   string `json:"status" enum:"ok,degraded" doc:"Overall service health"`
		Version  string `json:"version" doc:"Build identity"`
		Database string `json:"database" enum:"ok,unreachable" doc:"Database connectivity"`
	}
}

func (s *Server) registerHealth() {
	huma.Register(s.API, huma.Operation{
		OperationID: "health",
		Method:      http.MethodGet,
		Path:        "/health",
		Summary:     "Service health",
		Description: "Liveness and dependency status. Does not require authentication.",
		Tags:        []string{"system"},
	}, func(ctx context.Context, _ *struct{}) (*HealthOutput, error) {
		out := &HealthOutput{}
		out.Body.Version = version.String()
		out.Body.Status = "ok"
		out.Body.Database = "ok"

		if s.queries == nil {
			out.Body.Status = "degraded"
			out.Body.Database = "unreachable"
			return out, nil
		}

		// A real query rather than Ping: this proves a full round trip through
		// the pool, whereas Ping can succeed against a socket that is accepting
		// connections but not yet serving queries.
		qCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if _, err := s.queries.ServerTime(qCtx); err != nil {
			s.deps.Log.Warn("health check: database query failed", "error", err)
			out.Body.Status = "degraded"
			out.Body.Database = "unreachable"
		}
		return out, nil
	})
}

// ---- static client ---------------------------------------------------------

// registerWeb serves the built client, falling back to index.html so that
// client-side routes deep-link correctly on refresh.
func (s *Server) registerWeb() {
	fileServer := http.FileServer(s.deps.WebFS)

	s.Router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		f, err := s.deps.WebFS.Open(r.URL.Path)
		if err == nil {
			defer f.Close()
			if st, statErr := f.Stat(); statErr == nil && !st.IsDir() {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		s.serveIndex(w, r)
	})
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := s.deps.WebFS.Open("/index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// The shell must not be cached: it references hashed asset filenames that
	// change on every deploy.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", st.ModTime(), f)
}
