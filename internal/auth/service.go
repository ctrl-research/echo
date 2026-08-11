// Package auth implements sign-in — Google, a generic OIDC provider, and
// optional local accounts — plus server-side sessions and the request context
// plumbing that carries an authenticated user to handlers.
//
// See docs/design.md, "Authentication".
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jonathanng/echo/internal/db/dbgen"
)

// ErrAccountDisabled is returned when a known user has been disabled.
var ErrAccountDisabled = errors.New("auth: account disabled")

// Options configures the auth service. Providers are enabled by being
// configured: absent credentials simply mean the button is not offered.
type Options struct {
	// BaseURL is the instance's public URL. OAuth redirect URIs derive from
	// it, and it decides whether cookies are marked Secure.
	BaseURL string

	GoogleClientID     string
	GoogleClientSecret string

	// Generic OIDC provider. Issuer, client id, and secret must be set
	// together; OIDCName is the button label.
	OIDCIssuerURL    string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCName         string

	// LocalAuth enables email/password sign-in. Off by default: it exists for
	// development and for a break-glass administrator on an instance whose
	// identity provider is unreachable.
	LocalAuth bool

	// AllowedEmails may create accounts after the first user exists. The first
	// user to sign in is always allowed and becomes the administrator.
	AllowedEmails []string

	SessionTTL time.Duration
}

type Service struct {
	q   *dbgen.Queries
	log *slog.Logger

	google *ssoProvider // nil when Google sign-in is not configured
	oidc   *ssoProvider // nil when no generic OIDC provider is configured

	localAuth     bool
	secureCookies bool
	sessionTTL    time.Duration
	allowedEmails map[string]bool
}

// NewService performs OIDC discovery for each configured provider, so a
// misconfigured issuer fails at startup rather than at the first sign-in
// attempt.
func NewService(ctx context.Context, q *dbgen.Queries, log *slog.Logger, opts Options) (*Service, error) {
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = 30 * 24 * time.Hour
	}
	baseURL := strings.TrimSuffix(opts.BaseURL, "/")

	allowed := make(map[string]bool, len(opts.AllowedEmails))
	for _, e := range opts.AllowedEmails {
		if e = strings.TrimSpace(strings.ToLower(e)); e != "" {
			allowed[e] = true
		}
	}

	s := &Service{
		q:   q,
		log: log,
		// Derived from BaseURL rather than configured separately: a Secure
		// cookie on an http instance is silently dropped by the browser, and
		// an https instance must never send the cookie in the clear. There is
		// no combination where the two should disagree.
		secureCookies: strings.HasPrefix(baseURL, "https://"),
		localAuth:     opts.LocalAuth,
		sessionTTL:    opts.SessionTTL,
		allowedEmails: allowed,
	}

	if opts.GoogleClientID != "" {
		p, err := newGoogleProvider(ctx, opts, baseURL)
		if err != nil {
			return nil, err
		}
		p.upsert = s.upsertGoogle
		s.google = p
	}
	if opts.OIDCIssuerURL != "" {
		name := opts.OIDCName
		if name == "" {
			name = "SSO"
		}
		// Self-hosted IdPs often omit email_verified entirely, so only an
		// explicit false is rejected for the generic provider.
		p, err := newSSOProvider(ctx, "oidc", name, opts.OIDCIssuerURL,
			opts.OIDCClientID, opts.OIDCClientSecret, baseURL, false)
		if err != nil {
			return nil, err
		}
		p.upsert = s.upsertOIDC
		s.oidc = p
	}

	if s.google == nil && s.oidc == nil && !s.localAuth {
		log.Warn("no sign-in method is configured; nobody can sign in. " +
			"Set ECHO_GOOGLE_CLIENT_ID/SECRET, the ECHO_OIDC_* variables, " +
			"or ECHO_LOCAL_AUTH=true")
	}
	return s, nil
}

// LocalAuthEnabled reports whether email/password sign-in is available.
func (s *Service) LocalAuthEnabled() bool { return s.localAuth }

// SecureCookies reports whether cookies carry the Secure attribute.
func (s *Service) SecureCookies() bool { return s.secureCookies }

// Provider describes one enabled sign-in method for the client.
type Provider struct {
	// Key is "google", "oidc", or "local".
	Key string `json:"key"`
	// Name is the button label.
	Name string `json:"name"`
	// StartURL is where the browser navigates to begin the flow. Empty for
	// local, which posts credentials to the API instead.
	StartURL string `json:"startUrl,omitempty"`
}

// Providers lists the enabled sign-in methods, in the order the client should
// present them.
func (s *Service) Providers() []Provider {
	out := []Provider{}
	if s.google != nil {
		out = append(out, Provider{Key: "google", Name: "Google", StartURL: "/auth/google"})
	}
	if s.oidc != nil {
		out = append(out, Provider{Key: "oidc", Name: s.oidc.display, StartURL: "/auth/oidc"})
	}
	if s.localAuth {
		out = append(out, Provider{Key: "local", Name: "Email and password"})
	}
	return out
}

// RegisterRoutes mounts the browser-facing redirect endpoints.
//
// These live at the site root rather than under /api/v1 because they are
// browser navigations, not API calls: the user's browser is redirected here by
// the identity provider, and the response is a redirect rather than JSON. The
// JSON surface (/auth/me, /auth/logout, /auth/providers) stays in the API.
func (s *Service) RegisterRoutes(mux interface {
	Get(pattern string, h http.HandlerFunc)
}) {
	if s.google != nil {
		mux.Get("/auth/google", s.handleStart(s.google))
		mux.Get("/auth/google/callback", s.handleCallback(s.google))
	}
	if s.oidc != nil {
		mux.Get("/auth/oidc", s.handleStart(s.oidc))
		mux.Get("/auth/oidc/callback", s.handleCallback(s.oidc))
	}
}

// ---- account resolution ----------------------------------------------------

// upsertGoogle and upsertOIDC differ only in which subject column they use.
func (s *Service) upsertGoogle(ctx context.Context, id providerIdentity) (dbgen.User, error) {
	return s.upsert(ctx, id, providerGoogle)
}

func (s *Service) upsertOIDC(ctx context.Context, id providerIdentity) (dbgen.User, error) {
	return s.upsert(ctx, id, providerOIDC)
}

type providerKind int

const (
	providerGoogle providerKind = iota
	providerOIDC
)

// upsert resolves a verified provider identity to an Echo user, in three
// steps: match the provider subject, else link to an existing account with the
// same email, else create a new account subject to the allowlist.
//
// Matching on subject before email is deliberate. The subject is the IdP's
// stable identifier and survives an email change at the provider; matching on
// email first would strand a renamed user with a new account.
func (s *Service) upsert(ctx context.Context, id providerIdentity, kind providerKind) (dbgen.User, error) {
	var avatar *string
	if id.Picture != "" {
		avatar = &id.Picture
	}
	email := strings.ToLower(strings.TrimSpace(id.Email))

	// 1. Known subject: refresh the profile the IdP owns and sign in.
	var (
		user dbgen.User
		err  error
	)
	if kind == providerGoogle {
		user, err = s.q.GetUserByGoogleSub(ctx, &id.Subject)
	} else {
		user, err = s.q.GetUserByOIDCSub(ctx, &id.Subject)
	}
	switch {
	case err == nil:
		if user.DisabledAt.Valid {
			return dbgen.User{}, ErrAccountDisabled
		}
		return s.q.UpdateProfile(ctx, dbgen.UpdateProfileParams{
			ID: user.ID, DisplayName: id.Name, AvatarUrl: avatar,
		})
	case !errors.Is(err, pgx.ErrNoRows):
		return dbgen.User{}, fmt.Errorf("lookup by subject: %w", err)
	}

	// 2. Same email, different or first provider: link rather than duplicate.
	user, err = s.q.GetUserByEmail(ctx, email)
	switch {
	case err == nil:
		if user.DisabledAt.Valid {
			return dbgen.User{}, ErrAccountDisabled
		}
		if kind == providerGoogle {
			return s.q.LinkGoogleSub(ctx, dbgen.LinkGoogleSubParams{
				ID: user.ID, GoogleSub: id.Subject, DisplayName: id.Name, AvatarUrl: avatar,
			})
		}
		return s.q.LinkOIDCSub(ctx, dbgen.LinkOIDCSubParams{
			ID: user.ID, OidcSub: id.Subject, DisplayName: id.Name, AvatarUrl: avatar,
		})
	case !errors.Is(err, pgx.ErrNoRows):
		return dbgen.User{}, fmt.Errorf("lookup by email: %w", err)
	}

	// 3. New account. The first user to sign in claims the instance and
	// becomes administrator; everyone after must be on the allowlist.
	count, err := s.q.CountUsers(ctx)
	if err != nil {
		return dbgen.User{}, fmt.Errorf("count users: %w", err)
	}
	if count > 0 && !s.allowedEmails[email] {
		return dbgen.User{}, ErrEmailNotAllowed
	}

	params := dbgen.CreateUserParams{
		Email:       email,
		DisplayName: id.Name,
		AvatarUrl:   avatar,
		Role:        dbgen.UserRoleUser,
	}
	if count == 0 {
		params.Role = dbgen.UserRoleAdmin
	}
	if kind == providerGoogle {
		params.GoogleSub = &id.Subject
	} else {
		params.OidcSub = &id.Subject
	}

	created, err := s.q.CreateUser(ctx, params)
	if err != nil {
		return dbgen.User{}, fmt.Errorf("create user: %w", err)
	}
	s.log.Info("account created", "email", created.Email, "role", created.Role)
	return created, nil
}

// ---- sessions --------------------------------------------------------------

// SignIn creates a session for user and writes the session and CSRF cookies.
func (s *Service) SignIn(ctx context.Context, w http.ResponseWriter, user dbgen.User, userAgent string) error {
	token, digest, err := NewToken()
	if err != nil {
		return err
	}
	csrf, err := NewCSRFToken()
	if err != nil {
		return err
	}

	expires := time.Now().Add(s.sessionTTL)
	params := dbgen.CreateSessionParams{
		UserID:    user.ID,
		TokenHash: digest,
		CsrfToken: csrf,
		ExpiresAt: expires,
	}
	if userAgent != "" {
		params.UserAgent = &userAgent
	}
	if ip, ok := ClientIPFrom(ctx); ok {
		params.Ip = &ip
	}
	if _, err := s.q.CreateSession(ctx, params); err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	for _, c := range s.SessionCookies(token, csrf, expires) {
		http.SetCookie(w, &c)
	}
	return nil
}

// SessionCookies returns the pair of cookies that carry a session.
func (s *Service) SessionCookies(token, csrf string, expires time.Time) []http.Cookie {
	return []http.Cookie{
		{
			Name: SessionCookie, Value: token, Path: "/", Expires: expires,
			// Unreadable by script, yet still sent by <audio src> and by the
			// service worker replaying a range request — which is exactly why
			// sessions are cookies here and not bearer tokens.
			HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
		},
		{
			Name: CSRFCookie, Value: csrf, Path: "/", Expires: expires,
			// Deliberately readable: the client echoes it back in a header,
			// and that asymmetry is what the double-submit pattern relies on.
			HttpOnly: false, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
		},
	}
}

// ExpiredCookies clears both cookies.
func (s *Service) ExpiredCookies() []http.Cookie {
	return []http.Cookie{
		{
			Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
		},
		{
			Name: CSRFCookie, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: false, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
		},
	}
}

// GCLoop deletes expired sessions until ctx is cancelled. Without it the table
// grows without bound, since nothing else removes a session that simply aged
// out rather than being signed out.
func (s *Service) GCLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := s.q.DeleteExpiredSessions(ctx)
			if err != nil {
				s.log.Error("session gc failed", "error", err)
			} else if n > 0 {
				s.log.Info("session gc", "deleted", n)
			}
		}
	}
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }
