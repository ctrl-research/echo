package api

import (
	"errors"
	"net/http"

	"github.com/jonathanng/echo/internal/auth"
)

// sessionMiddleware resolves the session cookie to an Identity and attaches it
// to the request context. It never rejects a request: anonymous callers simply
// arrive with no identity, and the route guards decide what that means. This
// keeps public endpoints (health, providers, login) on the same path.
func (s *Server) sessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(auth.SessionCookie)
		if err != nil || cookie.Value == "" {
			next.ServeHTTP(w, r)
			return
		}

		identity, err := s.deps.Auth.ResolveSession(r.Context(), cookie.Value)
		if err != nil {
			if !errors.Is(err, auth.ErrNoSession) {
				s.deps.Log.Error("session lookup failed", "error", err)
			}
			// Expired, revoked, or belonging to a disabled account. Clear the
			// cookie so the browser stops presenting a token that will never
			// work again.
			for _, c := range s.deps.Auth.ExpiredCookies() {
				http.SetCookie(w, &c)
			}
			next.ServeHTTP(w, r)
			return
		}

		next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), identity)))
	})
}

// csrfMiddleware enforces the double-submit pattern on state-changing requests.
//
// SameSite=Lax on the session cookie already blocks the classic cross-site form
// POST. This is defence in depth for what Lax does not cover — notably a
// compromised or attacker-controlled subdomain, which is same-site for cookie
// purposes.
//
// Only authenticated requests are checked. An anonymous request has no
// privileges to abuse, and sign-in issues the token pair it will need.
func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		identity := auth.FromContext(r.Context())
		if identity == nil {
			next.ServeHTTP(w, r)
			return
		}

		presented := r.Header.Get(auth.CSRFHeader)
		if presented == "" || !auth.ConstantTimeEqual(presented, identity.CSRFToken) {
			s.deps.Log.Warn("csrf check failed",
				"path", r.URL.Path, "user", identity.Email, "presented", presented != "")
			apiError(http.StatusForbidden, "Forbidden",
				"Missing or invalid "+auth.CSRFHeader+" header.")(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}
