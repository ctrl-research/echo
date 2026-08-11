//go:build integration

package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jonathanng/echo/internal/auth"
	"github.com/jonathanng/echo/internal/db/dbgen"
	"github.com/jonathanng/echo/internal/dbtest"
)

func TestMain(m *testing.M) { os.Exit(dbtest.Main(m)) }

const clientID = "echo-test-client"

// newOIDCService wires the generic OIDC provider at the fake IdP.
func newOIDCService(t *testing.T, idp *fakeIdP, opts ...func(*auth.Options)) (*auth.Service, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.New(t)

	o := auth.Options{
		BaseURL:          "http://localhost:8080",
		OIDCIssuerURL:    idp.srv.URL,
		OIDCClientID:     clientID,
		OIDCClientSecret: "shhh",
		OIDCName:         "Authentik",
	}
	for _, fn := range opts {
		fn(&o)
	}

	svc, err := auth.NewService(context.Background(), dbgen.New(pool), dbtest.DiscardLogger(), o)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, pool
}

// signInRoundTrip drives the full browser flow: start, follow the authorize
// redirect's state back into the callback, and return the callback response.
func signInRoundTrip(t *testing.T, svc *auth.Service, path string) *httptest.ResponseRecorder {
	t.Helper()

	router := chiLike(svc)

	start := httptest.NewRecorder()
	router.ServeHTTP(start, httptest.NewRequest(http.MethodGet, path, nil))
	if start.Code != http.StatusFound {
		t.Fatalf("start: status = %d, want 302; body: %s", start.Code, start.Body)
	}

	authorizeURL, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	state := authorizeURL.Query().Get("state")
	if state == "" {
		t.Fatal("authorize url carries no state parameter")
	}

	cb := httptest.NewRequest(http.MethodGet, path+"/callback?state="+state+"&code=any", nil)
	for _, c := range start.Result().Cookies() {
		cb.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, cb)
	return rec
}

// chiLike adapts Service.RegisterRoutes to a plain ServeMux for these tests,
// which do not need the rest of the HTTP stack.
func chiLike(svc *auth.Service) http.Handler {
	mux := http.NewServeMux()
	svc.RegisterRoutes(muxAdapter{mux})
	return mux
}

type muxAdapter struct{ mux *http.ServeMux }

func (m muxAdapter) Get(pattern string, h http.HandlerFunc) {
	m.mux.HandleFunc("GET "+pattern, h)
}

// redirectError returns the error code the callback redirected with, or "" on
// success.
func redirectError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	return loc.Query().Get("error")
}

func userByEmail(t *testing.T, pool *pgxpool.Pool, email string) dbgen.User {
	t.Helper()
	user, err := dbgen.New(pool).GetUserByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("load %s: %v", email, err)
	}
	return user
}

// ---- happy path ------------------------------------------------------------

// The first person to sign in claims the instance and becomes administrator.
func TestFirstSignInCreatesAdmin(t *testing.T) {
	idp := newFakeIdP(t)
	svc, pool := newOIDCService(t, idp)
	idp.setClaims(clientID, map[string]any{
		"sub": "authentik-abc", "email": "jonathan@example.com",
		"email_verified": true, "name": "Jonathan", "picture": "https://img.example/a.png",
	})

	rec := signInRoundTrip(t, svc, "/auth/oidc")
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302; body: %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("redirected to %q, want %q (body: %s)", loc, "/", rec.Body)
	}

	user := userByEmail(t, pool, "jonathan@example.com")
	if user.Role != dbgen.UserRoleAdmin {
		t.Errorf("role = %q, want admin for the first user", user.Role)
	}
	if user.OidcSub == nil || *user.OidcSub != "authentik-abc" {
		t.Errorf("oidc_sub = %v, want authentik-abc", user.OidcSub)
	}
	if user.DisplayName != "Jonathan" {
		t.Errorf("display name = %q, want %q", user.DisplayName, "Jonathan")
	}
	if user.PasswordHash != nil {
		t.Error("an SSO account was given a password hash")
	}

	// A session cookie must have been issued.
	var sessionSet bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie && c.Value != "" {
			sessionSet = true
			if !c.HttpOnly {
				t.Error("session cookie is not HttpOnly")
			}
		}
	}
	if !sessionSet {
		t.Error("no session cookie was set")
	}
}

// The provider subject, not the email, is the identity: a user who changes
// their address at the IdP must land on the same account.
func TestSubjectSurvivesEmailChange(t *testing.T) {
	idp := newFakeIdP(t)
	svc, pool := newOIDCService(t, idp)

	idp.setClaims(clientID, map[string]any{
		"sub": "stable-sub", "email": "old@example.com", "email_verified": true, "name": "User",
	})
	signInRoundTrip(t, svc, "/auth/oidc")
	first := userByEmail(t, pool, "old@example.com")

	idp.setClaims(clientID, map[string]any{
		"sub": "stable-sub", "email": "new@example.com", "email_verified": true, "name": "User",
	})
	rec := signInRoundTrip(t, svc, "/auth/oidc")
	if got := redirectError(t, rec); got != "" {
		t.Fatalf("second sign-in failed: %s", got)
	}

	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Errorf("user count = %d, want 1; the subject did not match an existing account", count)
	}

	same, err := dbgen.New(pool).GetUser(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	// The email column is not updated from claims — the account is identified
	// by subject, and rewriting the address on every sign-in would fight any
	// local edit. What matters is that no second account appeared.
	if same.ID != first.ID {
		t.Errorf("user id changed from %s to %s", first.ID, same.ID)
	}
}

// Signing in with a second provider must link to the existing account rather
// than create a duplicate for the same person.
func TestSecondProviderLinksByEmail(t *testing.T) {
	idp := newFakeIdP(t)
	svc, pool := newOIDCService(t, idp, func(o *auth.Options) {
		// Point Google discovery at the same fake IdP so both providers exist.
		auth.SetGoogleIssuerForTest(idp.srv.URL)
		o.GoogleClientID = clientID
		o.GoogleClientSecret = "shhh"
	})
	t.Cleanup(func() { auth.SetGoogleIssuerForTest("https://accounts.google.com") })

	idp.setClaims(clientID, map[string]any{
		"sub": "oidc-1", "email": "shared@example.com", "email_verified": true, "name": "Shared",
	})
	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/oidc")); got != "" {
		t.Fatalf("oidc sign-in failed: %s", got)
	}

	idp.setClaims(clientID, map[string]any{
		"sub": "google-1", "email": "shared@example.com", "email_verified": true, "name": "Shared",
	})
	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/google")); got != "" {
		t.Fatalf("google sign-in failed: %s", got)
	}

	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Fatalf("user count = %d, want 1; the accounts did not link", count)
	}

	user := userByEmail(t, pool, "shared@example.com")
	if user.OidcSub == nil || *user.OidcSub != "oidc-1" {
		t.Errorf("oidc_sub = %v, want oidc-1", user.OidcSub)
	}
	if user.GoogleSub == nil || *user.GoogleSub != "google-1" {
		t.Errorf("google_sub = %v, want google-1", user.GoogleSub)
	}
}

// ---- allowlist -------------------------------------------------------------

func TestAllowlistGatesSignupsAfterTheFirstUser(t *testing.T) {
	idp := newFakeIdP(t)
	svc, pool := newOIDCService(t, idp, func(o *auth.Options) {
		o.AllowedEmails = []string{"Invited@Example.com"}
	})

	idp.setClaims(clientID, map[string]any{
		"sub": "s1", "email": "first@example.com", "email_verified": true, "name": "First",
	})
	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/oidc")); got != "" {
		t.Fatalf("first sign-in failed: %s", got)
	}

	// On the list, though with different casing.
	idp.setClaims(clientID, map[string]any{
		"sub": "s2", "email": "invited@example.com", "email_verified": true, "name": "Invited",
	})
	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/oidc")); got != "" {
		t.Errorf("allowlisted sign-up was refused: %s", got)
	}

	// Not on the list.
	idp.setClaims(clientID, map[string]any{
		"sub": "s3", "email": "stranger@example.com", "email_verified": true, "name": "Stranger",
	})
	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/oidc")); got != "not_allowed" {
		t.Errorf("error = %q, want not_allowed", got)
	}

	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 2 {
		t.Errorf("user count = %d, want 2", count)
	}
}

// An existing account signs in regardless of the allowlist: the list gates
// sign-up, not sign-in.
func TestAllowlistDoesNotBlockExistingUsers(t *testing.T) {
	idp := newFakeIdP(t)
	svc, _ := newOIDCService(t, idp)

	idp.setClaims(clientID, map[string]any{
		"sub": "s1", "email": "first@example.com", "email_verified": true, "name": "First",
	})
	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/oidc")); got != "" {
		t.Fatalf("first sign-in failed: %s", got)
	}
	// Same person again, with an empty allowlist in force.
	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/oidc")); got != "" {
		t.Errorf("existing user was refused by the allowlist: %s", got)
	}
}

func TestDisabledAccountCannotSignIn(t *testing.T) {
	idp := newFakeIdP(t)
	svc, pool := newOIDCService(t, idp)

	idp.setClaims(clientID, map[string]any{
		"sub": "s1", "email": "first@example.com", "email_verified": true, "name": "First",
	})
	signInRoundTrip(t, svc, "/auth/oidc")

	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET disabled_at = now()`); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/oidc")); got != "disabled" {
		t.Errorf("error = %q, want disabled", got)
	}
}

// ---- token and flow validation ---------------------------------------------

// An IdP that says the address is unverified must never be believed, even for
// the lenient generic provider.
func TestExplicitlyUnverifiedEmailIsRejected(t *testing.T) {
	idp := newFakeIdP(t)
	svc, _ := newOIDCService(t, idp)

	idp.setClaims(clientID, map[string]any{
		"sub": "s1", "email": "spoof@example.com", "email_verified": false, "name": "Spoof",
	})
	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/oidc")); got != "email_unverified" {
		t.Errorf("error = %q, want email_unverified", got)
	}
}

// Self-hosted providers routinely omit email_verified; the generic provider
// tolerates that, while Google does not.
func TestMissingEmailVerifiedIsToleratedForGenericOIDC(t *testing.T) {
	idp := newFakeIdP(t)
	svc, _ := newOIDCService(t, idp)

	idp.setClaims(clientID, map[string]any{
		"sub": "s1", "email": "nobody@example.com", "name": "Nobody",
	})
	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/oidc")); got != "" {
		t.Errorf("generic OIDC refused a token with no email_verified claim: %s", got)
	}
}

func TestMissingEmailVerifiedIsRejectedForGoogle(t *testing.T) {
	idp := newFakeIdP(t)
	auth.SetGoogleIssuerForTest(idp.srv.URL)
	t.Cleanup(func() { auth.SetGoogleIssuerForTest("https://accounts.google.com") })

	pool := dbtest.New(t)
	svc, err := auth.NewService(context.Background(), dbgen.New(pool), dbtest.DiscardLogger(), auth.Options{
		BaseURL:            "http://localhost:8080",
		GoogleClientID:     clientID,
		GoogleClientSecret: "shhh",
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	idp.setClaims(clientID, map[string]any{
		"sub": "g1", "email": "nobody@example.com", "name": "Nobody",
	})
	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/google")); got != "email_unverified" {
		t.Errorf("error = %q, want email_unverified", got)
	}
}

// A token minted for a different client must not be accepted, or any site
// using the same IdP could forge a sign-in.
func TestWrongAudienceIsRejected(t *testing.T) {
	idp := newFakeIdP(t)
	svc, _ := newOIDCService(t, idp)

	idp.setClaims("some-other-client", map[string]any{
		"sub": "s1", "email": "attacker@example.com", "email_verified": true,
	})
	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/oidc")); got != "bad_id_token" {
		t.Errorf("error = %q, want bad_id_token", got)
	}
}

func TestTokenWithoutEmailIsRejected(t *testing.T) {
	idp := newFakeIdP(t)
	svc, _ := newOIDCService(t, idp)

	idp.setClaims(clientID, map[string]any{"sub": "s1", "email_verified": true})
	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/oidc")); got != "bad_claims" {
		t.Errorf("error = %q, want bad_claims", got)
	}
}

func TestMissingIDTokenIsRejected(t *testing.T) {
	idp := newFakeIdP(t)
	svc, _ := newOIDCService(t, idp)
	idp.omitIDToken = true
	idp.setClaims(clientID, map[string]any{"sub": "s1", "email": "a@example.com"})

	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/oidc")); got != "no_id_token" {
		t.Errorf("error = %q, want no_id_token", got)
	}
}

// The state parameter is the CSRF defence for the redirect: a callback whose
// state does not match the cookie must be refused.
func TestCallbackRejectsMismatchedState(t *testing.T) {
	idp := newFakeIdP(t)
	svc, _ := newOIDCService(t, idp)
	idp.setClaims(clientID, map[string]any{
		"sub": "s1", "email": "a@example.com", "email_verified": true,
	})
	router := chiLike(svc)

	start := httptest.NewRecorder()
	router.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/auth/oidc", nil))

	cb := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state=forged&code=any", nil)
	for _, c := range start.Result().Cookies() {
		cb.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, cb)

	if got := redirectError(t, rec); got != "invalid_state" {
		t.Errorf("error = %q, want invalid_state", got)
	}
}

// A callback carrying no flow cookies at all — a bare link someone was tricked
// into following — must be refused rather than attempt an exchange.
func TestCallbackWithoutCookiesIsRejected(t *testing.T) {
	idp := newFakeIdP(t)
	svc, _ := newOIDCService(t, idp)

	rec := httptest.NewRecorder()
	chiLike(svc).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state=x&code=y", nil))

	if got := redirectError(t, rec); got != "invalid_state" {
		t.Errorf("error = %q, want invalid_state", got)
	}
}

// The one-shot state and verifier cookies must be cleared by the callback so
// they cannot be replayed.
func TestFlowCookiesAreClearedByTheCallback(t *testing.T) {
	idp := newFakeIdP(t)
	svc, _ := newOIDCService(t, idp)
	idp.setClaims(clientID, map[string]any{
		"sub": "s1", "email": "a@example.com", "email_verified": true,
	})

	rec := signInRoundTrip(t, svc, "/auth/oidc")

	cleared := map[string]bool{}
	for _, c := range rec.Result().Cookies() {
		if strings.HasPrefix(c.Name, "echo_oauth_") {
			cleared[c.Name] = c.MaxAge < 0
		}
	}
	if len(cleared) != 2 {
		t.Fatalf("callback set %d oauth flow cookies, want 2 cleared: %v", len(cleared), cleared)
	}
	for name, ok := range cleared {
		if !ok {
			t.Errorf("cookie %s was not expired by the callback", name)
		}
	}
}

// The authorize redirect must carry a PKCE challenge; without it an
// intercepted authorization code could be redeemed by an attacker.
func TestAuthorizeURLUsesPKCE(t *testing.T) {
	idp := newFakeIdP(t)
	svc, _ := newOIDCService(t, idp)

	rec := httptest.NewRecorder()
	chiLike(svc).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/oidc", nil))

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	q := loc.Query()
	if q.Get("code_challenge") == "" {
		t.Error("authorize url has no code_challenge")
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", got)
	}
	if got := q.Get("client_id"); got != clientID {
		t.Errorf("client_id = %q, want %q", got, clientID)
	}
	if got := q.Get("redirect_uri"); got != "http://localhost:8080/auth/oidc/callback" {
		t.Errorf("redirect_uri = %q", got)
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Errorf("scope = %q, want it to include openid", q.Get("scope"))
	}
}

// ---- discovery -------------------------------------------------------------

// Issuers are exact-match identifiers and providers disagree about trailing
// slashes. When that is the only difference, startup should recover rather than
// refuse to boot over one character.
func TestIssuerTrailingSlashIsTolerated(t *testing.T) {
	idp := newFakeIdP(t)
	idp.issuer = idp.srv.URL + "/"
	pool := dbtest.New(t)

	// Configured without the slash; the provider advertises it with one.
	svc, err := auth.NewService(context.Background(), dbgen.New(pool), dbtest.DiscardLogger(), auth.Options{
		BaseURL:          "http://localhost:8080",
		OIDCIssuerURL:    idp.srv.URL,
		OIDCClientID:     clientID,
		OIDCClientSecret: "shhh",
	})
	if err != nil {
		t.Fatalf("discovery did not recover from a trailing-slash mismatch: %v", err)
	}

	idp.setClaims(clientID, map[string]any{
		"sub": "s1", "email": "a@example.com", "email_verified": true, "name": "A",
	})
	if got := redirectError(t, signInRoundTrip(t, svc, "/auth/oidc")); got != "" {
		t.Errorf("sign-in failed after issuer normalisation: %s", got)
	}
}

// A wrong issuer must fail at startup, not at somebody's first sign-in.
func TestUnreachableIssuerFailsAtStartup(t *testing.T) {
	pool := dbtest.New(t)
	_, err := auth.NewService(context.Background(), dbgen.New(pool), dbtest.DiscardLogger(), auth.Options{
		BaseURL:          "http://localhost:8080",
		OIDCIssuerURL:    "http://127.0.0.1:1/nope",
		OIDCClientID:     clientID,
		OIDCClientSecret: "shhh",
	})
	if err == nil {
		t.Fatal("NewService succeeded with an unreachable issuer")
	}
	if !strings.Contains(err.Error(), "discovery") {
		t.Errorf("error = %v, want it to mention discovery", err)
	}
}

// ---- provider listing ------------------------------------------------------

func TestProvidersReflectsConfiguration(t *testing.T) {
	idp := newFakeIdP(t)
	svc, _ := newOIDCService(t, idp, func(o *auth.Options) { o.LocalAuth = true })

	got := svc.Providers()
	if len(got) != 2 {
		t.Fatalf("providers = %+v, want oidc and local", got)
	}
	if got[0].Key != "oidc" || got[0].Name != "Authentik" {
		t.Errorf("first provider = %+v, want key oidc named Authentik", got[0])
	}
	if got[0].StartURL != "/auth/oidc" {
		t.Errorf("start url = %q, want /auth/oidc", got[0].StartURL)
	}
	if got[1].Key != "local" {
		t.Errorf("second provider = %+v, want local", got[1])
	}
}

func TestNoProvidersConfigured(t *testing.T) {
	pool := dbtest.New(t)
	svc, err := auth.NewService(context.Background(), dbgen.New(pool), dbtest.DiscardLogger(),
		auth.Options{BaseURL: "http://localhost:8080"})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if got := svc.Providers(); len(got) != 0 {
		t.Errorf("providers = %+v, want none", got)
	}
}

// Cookies must be Secure exactly when the instance is https, since a Secure
// cookie on http is silently dropped and an http cookie on https leaks.
func TestSecureCookiesFollowBaseURL(t *testing.T) {
	pool := dbtest.New(t)
	for baseURL, want := range map[string]bool{
		"http://localhost:8080":    false,
		"https://echo.example.com": true,
	} {
		svc, err := auth.NewService(context.Background(), dbgen.New(pool), dbtest.DiscardLogger(),
			auth.Options{BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewService(%s): %v", baseURL, err)
		}
		if got := svc.SecureCookies(); got != want {
			t.Errorf("BaseURL %s: SecureCookies = %v, want %v", baseURL, got, want)
		}
	}
}
