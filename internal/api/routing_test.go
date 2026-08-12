package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// fakeClient stands in for the built Vite output: a shell plus a hashed asset.
func fakeClient() http.FileSystem {
	return http.FS(fstest.MapFS{
		"index.html":         {Data: []byte("<!doctype html><title>Echo</title>")},
		"assets/app-abc.js":  {Data: []byte("console.log('echo')")},
		"assets/app-abc.css": {Data: []byte("body{}")},
	})
}

func newTestServer(t *testing.T, web http.FileSystem) *Server {
	t.Helper()
	// Pool is nil: these tests exercise routing only. Health degrades rather
	// than panicking, which is itself part of the contract.
	return New(Deps{Log: slog.New(slog.DiscardHandler), WebFS: web})
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// An unknown path under the API prefix must produce a JSON error, not the SPA
// shell. chi propagates the parent router's NotFound handler into mounted
// sub-routers, so without an explicit handler this answers 200 with HTML.
func TestUnknownAPIPathReturnsJSON404(t *testing.T) {
	s := newTestServer(t, fakeClient())

	rec := get(t, s, APIPrefix+"/does-not-exist")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	if strings.Contains(rec.Body.String(), "<!doctype") {
		t.Error("API 404 served the SPA shell")
	}
}

func TestAPIWrongMethodReturns405(t *testing.T) {
	s := newTestServer(t, fakeClient())

	rec := httptest.NewRecorder()
	s.Router.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, APIPrefix+"/health", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405; body: %s", rec.Code, rec.Body)
	}
}

// Client-side routes must serve the shell so a refresh or a shared deep link
// works, while real files are served as themselves.
func TestSPAFallback(t *testing.T) {
	s := newTestServer(t, fakeClient())

	for _, path := range []string{"/", "/albums", "/albums/123", "/search"} {
		rec := get(t, s, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), "<!doctype") {
			t.Errorf("GET %s: did not serve the shell; body: %s", path, rec.Body)
		}
	}
}

func TestStaticAssetIsServedVerbatim(t *testing.T) {
	s := newTestServer(t, fakeClient())

	rec := get(t, s, "/assets/app-abc.js")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "console.log('echo')" {
		t.Errorf("body = %q, want the asset contents", got)
	}
}

// The shell references hashed asset filenames that change every deploy, so it
// must never be cached; the assets themselves are immutable and may be.
func TestShellIsNotCached(t *testing.T) {
	s := newTestServer(t, fakeClient())

	if cc := get(t, s, "/albums").Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control on shell = %q, want %q", cc, "no-cache")
	}
}

// Without a built client the binary must still serve the API rather than
// panicking on a nil filesystem.
func TestNoWebClientStillServesAPI(t *testing.T) {
	s := newTestServer(t, nil)

	if rec := get(t, s, APIPrefix+"/health"); rec.Code != http.StatusOK {
		t.Errorf("health status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if rec := get(t, s, "/albums"); rec.Code != http.StatusNotFound {
		t.Errorf("SPA path status = %d, want 404 when no client is embedded", rec.Code)
	}
}

// Health must answer rather than fail when the database is absent — it is what
// readiness probes call, including before the pool exists.
func TestHealthDegradesWithoutPool(t *testing.T) {
	s := newTestServer(t, nil)

	rec := get(t, s, APIPrefix+"/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"degraded"`) {
		t.Errorf("body = %s, want status degraded", body)
	}
}

// Audio players issue HEAD against media URLs before ranged reads, and the
// container healthcheck previously depended on HEAD too. chi answers 405 for
// HEAD unless the GetHead middleware is installed.
func TestHeadIsRoutedToGet(t *testing.T) {
	s := newTestServer(t, fakeClient())

	rec := httptest.NewRecorder()
	s.Router.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, APIPrefix+"/health", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("HEAD status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
}

// The OpenAPI document is the sole source of the client's types, so a missing
// operation silently removes it from the generated client rather than causing
// a visible failure. Registration must therefore never depend on runtime
// dependencies such as the database pool — `echo openapi` runs without one.
func TestOpenAPIContainsEveryOperation(t *testing.T) {
	// Deliberately no Pool: this is exactly how the openapi subcommand builds
	// the server.
	s := newTestServer(t, nil)

	spec, err := s.OpenAPI()
	if err != nil {
		t.Fatalf("OpenAPI: %v", err)
	}
	rendered := string(spec)

	for _, want := range []string{
		"/health",
		"/auth/login", "/auth/logout", "/auth/me", "/auth/password",
		"/admin/users", "/admin/users/{id}",
	} {
		if !strings.Contains(rendered, want+":") {
			t.Errorf("OpenAPI document is missing path %q", want)
		}
	}
	for _, want := range []string{
		"operationId: health",
		"operationId: login",
		"operationId: createUser",
		"operationId: updateUser",
		"operationId: deleteUser",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("OpenAPI document is missing %q", want)
		}
	}
}

// Without a pool, database-backed operations must fail cleanly rather than
// panicking on a nil querier.
func TestOperationsReport503WithoutDatabase(t *testing.T) {
	s := newTestServer(t, nil)

	for _, path := range []string{"/auth/me", "/admin/users"} {
		rec := get(t, s, APIPrefix+path)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s: status = %d, want 503; body: %s", path, rec.Code, rec.Body)
		}
	}
}

// Authorisation is default-deny, decided by an allowlist of public paths. This
// asserts the allowlist is exactly what we think it is, so an endpoint added
// later cannot quietly become anonymous — the failure mode that let the entire
// library be readable without a session.
func TestOnlyExpectedPathsArePublic(t *testing.T) {
	want := map[string]bool{
		"/health":         true,
		"/auth/providers": true,
		"/auth/login":     true,
	}

	if len(publicPaths) != len(want) {
		t.Fatalf("publicPaths = %v, want exactly %v", publicPaths, want)
	}
	for path := range want {
		if !publicPaths[path] {
			t.Errorf("%s should be public but is not", path)
		}
	}
	for path := range publicPaths {
		if !want[path] {
			t.Errorf("%s is public and should not be; add it here deliberately or "+
				"remove it from publicPaths", path)
		}
	}
}
