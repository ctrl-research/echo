//go:build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jonathanng/echo/internal/api"
	"github.com/jonathanng/echo/internal/auth"
	"github.com/jonathanng/echo/internal/blobstore"
	"github.com/jonathanng/echo/internal/db/dbgen"
	"github.com/jonathanng/echo/internal/jobs"
	"github.com/jonathanng/echo/internal/library"
)

const (
	adminEmail    = "admin@example.com"
	adminPassword = "bootstrap-password"
)

// harness runs the API over a real HTTP listener with a cookie jar, so cookie
// attributes, redirects and header plumbing behave exactly as they will in a
// browser. Secure cookies are off because httptest serves plain http — the
// same setting a developer uses against localhost.
type harness struct {
	t      *testing.T
	pool   *pgxpool.Pool
	server *httptest.Server
	client *http.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool := newTestDB(t)

	// Local sign-in is enabled here so these tests can exercise the session,
	// CSRF, and authorisation layers directly. The SSO flows themselves are
	// covered against a fake identity provider in internal/auth.
	authSvc, err := auth.NewService(context.Background(), dbgen.New(pool), discardLogger(),
		auth.Options{BaseURL: "http://localhost:8080", LocalAuth: true})
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}

	blobs, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blobstore: %v", err)
	}
	srv := api.New(api.Deps{
		Pool:    pool,
		Log:     discardLogger(),
		Auth:    authSvc,
		Library: library.NewService(pool, jobs.New(pool, discardLogger()), blobs, discardLogger()),
	})
	ts := httptest.NewServer(srv.Router)
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}

	h := &harness{t: t, pool: pool, server: ts, client: &http.Client{Jar: jar}}
	if err := auth.BootstrapLocalAdmin(context.Background(), dbgen.New(pool),
		discardLogger(), adminEmail, adminPassword); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	return h
}

// do issues a request, attaching the CSRF header from the jar when one is
// present — exactly what the browser client does.
func (h *harness) do(method, path string, body any) *http.Response {
	h.t.Helper()

	var rdr io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, h.server.URL+"/api/v1"+path, rdr)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token := h.csrfToken(); token != "" {
		req.Header.Set(auth.CSRFHeader, token)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// doWithoutCSRF deliberately omits the header, to prove the check is enforced.
func (h *harness) doWithoutCSRF(method, path string, body any) *http.Response {
	h.t.Helper()
	encoded, _ := json.Marshal(body)
	req, err := http.NewRequest(method, h.server.URL+"/api/v1"+path, bytes.NewReader(encoded))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (h *harness) csrfToken() string {
	u, _ := url.Parse(h.server.URL)
	for _, c := range h.client.Jar.Cookies(u) {
		if c.Name == auth.CSRFCookie {
			return c.Value
		}
	}
	return ""
}

func (h *harness) sessionCookie() string {
	u, _ := url.Parse(h.server.URL)
	for _, c := range h.client.Jar.Cookies(u) {
		if c.Name == auth.SessionCookie {
			return c.Value
		}
	}
	return ""
}

func (h *harness) login(email, password string) *http.Response {
	h.t.Helper()
	return h.do(http.MethodPost, "/auth/login", map[string]string{
		"email": email, "password": password,
	})
}

func (h *harness) loginAsAdmin() {
	h.t.Helper()
	resp := h.login(adminEmail, adminPassword)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("admin login failed: %d %s", resp.StatusCode, body)
	}
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, want, body)
	}
}

// ---- login -----------------------------------------------------------------

func TestBootstrapAdminAndLogin(t *testing.T) {
	h := newHarness(t)

	resp := h.login(adminEmail, adminPassword)
	assertStatus(t, resp, http.StatusOK)
	user := decode[api.UserDTO](t, resp)

	if user.Email != adminEmail {
		t.Errorf("email = %q, want %q", user.Email, adminEmail)
	}
	if user.Role != auth.RoleAdmin {
		t.Errorf("role = %q, want %q", user.Role, auth.RoleAdmin)
	}
	if h.sessionCookie() == "" {
		t.Error("no session cookie was set")
	}
	if h.csrfToken() == "" {
		t.Error("no CSRF cookie was set")
	}
}

// Bootstrap must not run against a populated table, or a lingering environment
// variable would recreate a deleted account on every restart.
func TestBootstrapIsSkippedWhenUsersExist(t *testing.T) {
	h := newHarness(t)

	err := auth.BootstrapLocalAdmin(context.Background(), dbgen.New(h.pool),
		discardLogger(), "second@example.com", "another-password")
	if err != nil {
		t.Fatalf("second bootstrap returned an error: %v", err)
	}

	var count int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Errorf("user count = %d, want 1", count)
	}
}

// Account enumeration: a wrong password and an unknown address must be
// indistinguishable in both status and body.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	h := newHarness(t)

	wrongPassword := h.login(adminEmail, "not-the-password")
	assertStatus(t, wrongPassword, http.StatusUnauthorized)
	wrongBody, _ := io.ReadAll(wrongPassword.Body)
	wrongPassword.Body.Close()

	unknownUser := h.login("nobody@example.com", "not-the-password")
	assertStatus(t, unknownUser, http.StatusUnauthorized)
	unknownBody, _ := io.ReadAll(unknownUser.Body)
	unknownUser.Body.Close()

	if string(wrongBody) != string(unknownBody) {
		t.Errorf("responses differ and leak account existence:\n wrong password: %s\n unknown user:  %s",
			wrongBody, unknownBody)
	}
	if h.sessionCookie() != "" {
		t.Error("a session cookie was set despite failed login")
	}
}

func TestLoginIsCaseInsensitiveOnEmail(t *testing.T) {
	h := newHarness(t)

	resp := h.login("ADMIN@EXAMPLE.COM", adminPassword)
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

func TestDisabledUserCannotLogIn(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	created := h.do(http.MethodPost, "/admin/users", map[string]string{
		"email": "bob@example.com", "password": "bob-password", "role": "user",
	})
	assertStatus(t, created, http.StatusCreated)
	bob := decode[api.UserDTO](t, created)

	patched := h.do(http.MethodPatch, "/admin/users/"+bob.ID, map[string]bool{"disabled": true})
	assertStatus(t, patched, http.StatusOK)
	patched.Body.Close()

	// Fresh jar: a disabled account must not be able to obtain a new session.
	fresh := newHarnessSharing(t, h)
	resp := fresh.login("bob@example.com", "bob-password")
	assertStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

// newHarnessSharing gives a second, independent cookie jar against the same
// server and database — the equivalent of a different browser.
func newHarnessSharing(t *testing.T, base *harness) *harness {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &harness{t: t, pool: base.pool, server: base.server,
		client: &http.Client{Jar: jar}}
}

// ---- session lifecycle -----------------------------------------------------

func TestMeRequiresAuthentication(t *testing.T) {
	h := newHarness(t)

	resp := h.do(http.MethodGet, "/auth/me", nil)
	assertStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()

	h.loginAsAdmin()

	resp = h.do(http.MethodGet, "/auth/me", nil)
	assertStatus(t, resp, http.StatusOK)
	if user := decode[api.UserDTO](t, resp); user.Email != adminEmail {
		t.Errorf("email = %q, want %s", user.Email, adminEmail)
	}
}

func TestLogoutRevokesTheSession(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()
	token := h.sessionCookie()

	resp := h.do(http.MethodPost, "/auth/logout", nil)
	assertStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()

	// The row must be gone, not merely un-cookied: a stolen token has to stop
	// working even if the client keeps presenting it.
	var count int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sessions WHERE token_hash = $1`,
		auth.TokenDigest(token)).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Error("session row survived logout")
	}

	resp = h.do(http.MethodGet, "/auth/me", nil)
	assertStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

// An expired session must be rejected even though the cookie is still present.
func TestExpiredSessionIsRejected(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	if _, err := h.pool.Exec(context.Background(),
		`UPDATE sessions SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatalf("expire sessions: %v", err)
	}

	resp := h.do(http.MethodGet, "/auth/me", nil)
	assertStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	// A second browser signed in as the same user.
	other := newHarnessSharing(t, h)
	other.loginAsAdmin()

	resp := h.do(http.MethodPost, "/auth/password", map[string]string{
		"currentPassword": adminPassword,
		"newPassword":     "a-brand-new-password",
	})
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// The other browser is signed out.
	otherResp := other.do(http.MethodGet, "/auth/me", nil)
	assertStatus(t, otherResp, http.StatusUnauthorized)
	otherResp.Body.Close()

	// The caller stays signed in on a freshly issued session.
	mine := h.do(http.MethodGet, "/auth/me", nil)
	assertStatus(t, mine, http.StatusOK)
	mine.Body.Close()

	// And the new password is what works now.
	fresh := newHarnessSharing(t, h)
	old := fresh.login(adminEmail, adminPassword)
	assertStatus(t, old, http.StatusUnauthorized)
	old.Body.Close()

	updated := fresh.login(adminEmail, "a-brand-new-password")
	assertStatus(t, updated, http.StatusOK)
	updated.Body.Close()
}

func TestChangePasswordRequiresCurrentPassword(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	resp := h.do(http.MethodPost, "/auth/password", map[string]string{
		"currentPassword": "wrong",
		"newPassword":     "a-brand-new-password",
	})
	assertStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()
}

// ---- CSRF ------------------------------------------------------------------

func TestCSRFRequiredForAuthenticatedMutations(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	resp := h.doWithoutCSRF(http.MethodPost, "/admin/users", map[string]string{
		"email": "eve@example.com", "password": "eve-password",
	})
	assertStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()

	// The same request with the header succeeds, proving the rejection was the
	// CSRF check and not something else about the request.
	ok := h.do(http.MethodPost, "/admin/users", map[string]string{
		"email": "eve@example.com", "password": "eve-password",
	})
	assertStatus(t, ok, http.StatusCreated)
	ok.Body.Close()
}

func TestCSRFRejectsAWrongToken(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	body, _ := json.Marshal(map[string]string{"email": "mallory@example.com", "password": "mallory-pw"})
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/v1/admin/users",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CSRFHeader, "not-the-right-token")

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertStatus(t, resp, http.StatusForbidden)
	resp.Body.Close()
}

// Reads must not require the header, or every GET would need it for no benefit.
func TestCSRFNotRequiredForReads(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/api/v1/auth/me", nil)
	resp, err := h.client.Do(req) // no CSRF header
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()
}

// ---- authorisation ---------------------------------------------------------

func TestAdminEndpointsRejectAnonymous(t *testing.T) {
	h := newHarness(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/users"},
		{http.MethodPost, "/admin/users"},
		{http.MethodGet, "/admin/users/00000000-0000-0000-0000-000000000000"},
	} {
		resp := h.do(tc.method, tc.path, map[string]string{
			"email": "x@example.com", "password": "xxxxxxxxxx",
		})
		if resp.StatusCode != http.StatusUnauthorized {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("%s %s: status = %d, want 401; body: %s",
				tc.method, tc.path, resp.StatusCode, body)
		}
		resp.Body.Close()
	}
}

func TestAdminEndpointsRejectNonAdmins(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	created := h.do(http.MethodPost, "/admin/users", map[string]string{
		"email": "regular@example.com", "password": "regular-password", "role": "user",
	})
	assertStatus(t, created, http.StatusCreated)
	created.Body.Close()

	regular := newHarnessSharing(t, h)
	resp := regular.login("regular@example.com", "regular-password")
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Authenticated but unprivileged: 403, not 401.
	listed := regular.do(http.MethodGet, "/admin/users", nil)
	assertStatus(t, listed, http.StatusForbidden)
	listed.Body.Close()

	// Their own identity endpoint still works.
	me := regular.do(http.MethodGet, "/auth/me", nil)
	assertStatus(t, me, http.StatusOK)
	if user := decode[api.UserDTO](t, me); user.Role != auth.RoleUser {
		t.Errorf("role = %q, want user", user.Role)
	}
}

// ---- user administration ---------------------------------------------------

func TestUserCRUD(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	created := h.do(http.MethodPost, "/admin/users", map[string]string{
		"email": "carol@example.com", "password": "carol-password", "role": "user",
	})
	assertStatus(t, created, http.StatusCreated)
	carol := decode[api.UserDTO](t, created)
	if carol.Role != auth.RoleUser || carol.Disabled {
		t.Errorf("created user = %+v, want an enabled user", carol)
	}

	listed := h.do(http.MethodGet, "/admin/users", nil)
	assertStatus(t, listed, http.StatusOK)
	list := decode[struct {
		Users []api.UserDTO `json:"users"`
	}](t, listed)
	if len(list.Users) != 2 {
		t.Errorf("listed %d users, want 2", len(list.Users))
	}

	promoted := h.do(http.MethodPatch, "/admin/users/"+carol.ID,
		map[string]string{"role": "admin"})
	assertStatus(t, promoted, http.StatusOK)
	if user := decode[api.UserDTO](t, promoted); user.Role != auth.RoleAdmin {
		t.Errorf("role after promotion = %q, want admin", user.Role)
	}

	deleted := h.do(http.MethodDelete, "/admin/users/"+carol.ID, nil)
	assertStatus(t, deleted, http.StatusNoContent)
	deleted.Body.Close()

	missing := h.do(http.MethodGet, "/admin/users/"+carol.ID, nil)
	assertStatus(t, missing, http.StatusNotFound)
	missing.Body.Close()
}

func TestDuplicateUsernameConflicts(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	first := h.do(http.MethodPost, "/admin/users", map[string]string{
		"email": "dave@example.com", "password": "dave-password",
	})
	assertStatus(t, first, http.StatusCreated)
	first.Body.Close()

	// Different case, same account: citext must make this a 409, not a 500.
	second := h.do(http.MethodPost, "/admin/users", map[string]string{
		"email": "DAVE@EXAMPLE.COM", "password": "another-password",
	})
	assertStatus(t, second, http.StatusConflict)
	second.Body.Close()
}

// Locking every administrator out has no recovery path short of hand-editing
// the database, so the last one must be undeletable and undemotable.
func TestLastAdminIsProtected(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	me := h.do(http.MethodGet, "/auth/me", nil)
	assertStatus(t, me, http.StatusOK)
	admin := decode[api.UserDTO](t, me)

	for name, body := range map[string]any{
		"demote":  map[string]string{"role": "user"},
		"disable": map[string]bool{"disabled": true},
	} {
		t.Run(name, func(t *testing.T) {
			resp := h.do(http.MethodPatch, "/admin/users/"+admin.ID, body)
			assertStatus(t, resp, http.StatusConflict)
			resp.Body.Close()
		})
	}

	t.Run("delete", func(t *testing.T) {
		resp := h.do(http.MethodDelete, "/admin/users/"+admin.ID, nil)
		assertStatus(t, resp, http.StatusConflict)
		resp.Body.Close()
	})

	// With a second admin present, the first becomes removable.
	created := h.do(http.MethodPost, "/admin/users", map[string]string{
		"email": "admin2@example.com", "password": "admin2-password", "role": "admin",
	})
	assertStatus(t, created, http.StatusCreated)
	created.Body.Close()

	resp := h.do(http.MethodDelete, "/admin/users/"+admin.ID, nil)
	assertStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()
}

// Changing someone's password, role, or enabled state must end their existing
// sessions; otherwise a revoked admin keeps their privileges until expiry.
func TestPrivilegeChangeRevokesTargetSessions(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	created := h.do(http.MethodPost, "/admin/users", map[string]string{
		"email": "frank@example.com", "password": "frank-password", "role": "admin",
	})
	assertStatus(t, created, http.StatusCreated)
	frank := decode[api.UserDTO](t, created)

	frankClient := newHarnessSharing(t, h)
	resp := frankClient.login("frank@example.com", "frank-password")
	assertStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	// Frank is an admin right now.
	listed := frankClient.do(http.MethodGet, "/admin/users", nil)
	assertStatus(t, listed, http.StatusOK)
	listed.Body.Close()

	demoted := h.do(http.MethodPatch, "/admin/users/"+frank.ID,
		map[string]string{"role": "user"})
	assertStatus(t, demoted, http.StatusOK)
	demoted.Body.Close()

	// His session is gone entirely, so he is anonymous rather than merely
	// unprivileged.
	after := frankClient.do(http.MethodGet, "/auth/me", nil)
	assertStatus(t, after, http.StatusUnauthorized)
	after.Body.Close()
}

func TestPasswordHashIsNeverSerialised(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()

	resp := h.do(http.MethodGet, "/admin/users", nil)
	assertStatus(t, resp, http.StatusOK)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, needle := range []string{"passwordHash", "password_hash", "$argon2id$"} {
		if bytes.Contains(body, []byte(needle)) {
			t.Errorf("response contains %q: %s", needle, body)
		}
	}
}

// The session cookie must be HttpOnly so that script cannot exfiltrate it, and
// the CSRF cookie must not be, since the client has to read and echo it.
func TestCookieAttributes(t *testing.T) {
	h := newHarness(t)

	resp := h.login(adminEmail, adminPassword)
	assertStatus(t, resp, http.StatusOK)
	defer resp.Body.Close()

	var session, csrf *http.Cookie
	for _, c := range resp.Cookies() {
		switch c.Name {
		case auth.SessionCookie:
			session = c
		case auth.CSRFCookie:
			csrf = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie in the response")
	}
	if csrf == nil {
		t.Fatal("no CSRF cookie in the response")
	}
	if !session.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if csrf.HttpOnly {
		t.Error("CSRF cookie is HttpOnly; the client cannot read it")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", session.SameSite)
	}
	if session.Value == "" {
		t.Error("session cookie is empty")
	}
}

// The stored value must be a digest, so that a database leak does not hand the
// attacker usable session tokens.
func TestSessionTokensAreStoredHashed(t *testing.T) {
	h := newHarness(t)
	h.loginAsAdmin()
	token := h.sessionCookie()

	var stored []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT token_hash FROM sessions`).Scan(&stored); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	if string(stored) == token {
		t.Error("session token is stored verbatim")
	}
	if got := auth.TokenDigest(token); string(got) != string(stored) {
		t.Error("stored value is not the SHA-256 digest of the token")
	}
}
