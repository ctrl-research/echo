package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/jonathanng/echo/internal/db/dbgen"
)

// ErrEmailNotAllowed is returned when a new sign-up is not on the allowlist.
var ErrEmailNotAllowed = errors.New("auth: email not allowed to sign up")

// ssoProvider is one OIDC identity provider — Google, or the generic
// ECHO_OIDC_* one (Authentik, Keycloak, Pocket ID, …). Both run the same
// authorization-code + PKCE flow and differ only in the discovery issuer, how
// strictly email verification is enforced, and which subject column links the
// account.
type ssoProvider struct {
	// key names the routes (/auth/{key}, /auth/{key}/callback) and cookies.
	key string
	// display is the label on the sign-in button ("Google", "Authentik", …).
	display  string
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	// requireVerifiedEmail rejects tokens that do not assert email_verified.
	// Google always sets the claim; self-hosted IdPs frequently omit it, so
	// the generic provider only rejects an explicit false.
	requireVerifiedEmail bool
	// upsert resolves a verified token identity to an Echo user.
	upsert func(ctx context.Context, id providerIdentity) (dbgen.User, error)
}

// providerIdentity is the subset of verified claims Echo consumes.
type providerIdentity struct {
	Subject string
	Email   string
	Name    string
	Picture string
}

func newSSOProvider(ctx context.Context, key, display, issuer, clientID, clientSecret, baseURL string, requireVerified bool) (*ssoProvider, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		// Issuers are exact-match identifiers and providers disagree about
		// trailing slashes — Authentik's ends with one, Keycloak's does not.
		// When that slash is the only difference, discovery has already proven
		// which form is canonical, so retry with it rather than refuse to start
		// over one character.
		alt := strings.TrimSuffix(issuer, "/")
		if alt == issuer {
			alt = issuer + "/"
		}
		if p2, err2 := oidc.NewProvider(ctx, alt); err2 == nil {
			slog.Warn("oidc issuer normalised to the provider's canonical form; update the configured issuer",
				"provider", key, "configured", issuer, "canonical", alt)
			provider, err = p2, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%s oidc discovery: %w", key, err)
	}

	return &ssoProvider{
		key:     key,
		display: display,
		oauth: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  baseURL + "/auth/" + key + "/callback",
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
		verifier:             provider.Verifier(&oidc.Config{ClientID: clientID}),
		requireVerifiedEmail: requireVerified,
	}, nil
}

func (p *ssoProvider) stateCookie() string    { return "echo_oauth_state_" + p.key }
func (p *ssoProvider) verifierCookie() string { return "echo_oauth_verifier_" + p.key }

// handleStart redirects the browser to the provider's authorization endpoint.
//
// The CSRF state and the PKCE verifier are stashed in short-lived HttpOnly
// cookies scoped to this provider's callback path, which is what carries them
// across the redirect round trip without server-side state.
func (s *Service) handleStart(p *ssoProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state, err := NewCSRFToken()
		if err != nil {
			http.Error(w, "sign-in failed", http.StatusInternalServerError)
			return
		}
		pkceVerifier := oauth2.GenerateVerifier()

		for name, value := range map[string]string{
			p.stateCookie():    state,
			p.verifierCookie(): pkceVerifier,
		} {
			http.SetCookie(w, &http.Cookie{
				Name:     name,
				Value:    value,
				Path:     "/auth/" + p.key,
				MaxAge:   600,
				HttpOnly: true,
				Secure:   s.secureCookies,
				SameSite: http.SameSiteLaxMode,
			})
		}

		http.Redirect(w, r,
			p.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(pkceVerifier)),
			http.StatusFound)
	}
}

// handleCallback completes the flow: verify state, exchange the code, verify
// the ID token, resolve it to a user, and start a session.
func (s *Service) handleCallback(p *ssoProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Cleared unconditionally: whether the flow succeeds or fails, these
		// one-shot values must not survive to be replayed.
		s.clearFlowCookies(w, p)

		stateCookie, stateErr := r.Cookie(p.stateCookie())
		verifierCookie, verifierErr := r.Cookie(p.verifierCookie())
		if stateErr != nil || verifierErr != nil ||
			!ConstantTimeEqual(r.URL.Query().Get("state"), stateCookie.Value) {
			s.failSignIn(w, r, http.StatusBadRequest, "invalid_state",
				"The sign-in request expired or did not match. Please try again.")
			return
		}
		if declined := r.URL.Query().Get("error"); declined != "" {
			s.log.Info("sso declined", "provider", p.key, "error", declined)
			s.failSignIn(w, r, http.StatusBadRequest, "declined",
				p.display+" sign-in was declined.")
			return
		}

		token, err := p.oauth.Exchange(r.Context(), r.URL.Query().Get("code"),
			oauth2.VerifierOption(verifierCookie.Value))
		if err != nil {
			s.log.Error("oauth exchange failed", "provider", p.key, "error", err)
			s.failSignIn(w, r, http.StatusBadGateway, "exchange_failed",
				p.display+" sign-in failed.")
			return
		}
		rawIDToken, ok := token.Extra("id_token").(string)
		if !ok {
			s.log.Error("oauth response had no id_token", "provider", p.key)
			s.failSignIn(w, r, http.StatusBadGateway, "no_id_token",
				p.display+" sign-in failed.")
			return
		}
		idToken, err := p.verifier.Verify(r.Context(), rawIDToken)
		if err != nil {
			s.log.Error("id token verification failed", "provider", p.key, "error", err)
			s.failSignIn(w, r, http.StatusBadGateway, "bad_id_token",
				p.display+" sign-in failed.")
			return
		}

		var claims struct {
			Email             string `json:"email"`
			EmailVerified     *bool  `json:"email_verified"`
			Name              string `json:"name"`
			PreferredUsername string `json:"preferred_username"`
			Picture           string `json:"picture"`
		}
		if err := idToken.Claims(&claims); err != nil || claims.Email == "" {
			s.log.Error("id token claims unusable", "provider", p.key, "error", err)
			s.failSignIn(w, r, http.StatusBadGateway, "bad_claims",
				p.display+" did not return an email address.")
			return
		}

		// Two separate rules. Strict providers must assert email_verified=true.
		// Lenient providers may omit the claim, but an explicit false is always
		// a refusal: an IdP saying "this address is unverified" must never be
		// treated as proof of the address.
		if claims.EmailVerified != nil && !*claims.EmailVerified {
			s.failSignIn(w, r, http.StatusForbidden, "email_unverified",
				"That "+p.display+" account's email address is not verified.")
			return
		}
		if p.requireVerifiedEmail && claims.EmailVerified == nil {
			s.failSignIn(w, r, http.StatusForbidden, "email_unverified",
				"That "+p.display+" account's email address is not verified.")
			return
		}

		name := claims.Name
		if name == "" {
			name = claims.PreferredUsername
		}

		user, err := p.upsert(r.Context(), providerIdentity{
			Subject: idToken.Subject,
			Email:   claims.Email,
			Name:    name,
			Picture: claims.Picture,
		})
		switch {
		case errors.Is(err, ErrEmailNotAllowed):
			s.log.Warn("sign-up refused by allowlist", "provider", p.key, "email", claims.Email)
			s.failSignIn(w, r, http.StatusForbidden, "not_allowed",
				"This Echo instance does not allow sign-ups for "+claims.Email+".")
			return
		case errors.Is(err, ErrAccountDisabled):
			s.failSignIn(w, r, http.StatusForbidden, "disabled",
				"That account has been disabled.")
			return
		case err != nil:
			s.log.Error("upsert sso user failed", "provider", p.key, "error", err)
			s.failSignIn(w, r, http.StatusInternalServerError, "server_error",
				"Sign-in failed.")
			return
		}

		if err := s.SignIn(r.Context(), w, user, r.UserAgent()); err != nil {
			s.log.Error("create session failed", "error", err)
			s.failSignIn(w, r, http.StatusInternalServerError, "server_error",
				"Sign-in failed.")
			return
		}
		s.log.Info("sign-in", "provider", p.key, "user", user.Email, "role", user.Role)
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

// failSignIn sends the browser back to the client with a machine-readable
// reason rather than rendering a dead-end error page. The user is mid-redirect
// in their browser, so a plain 403 body would strand them with no way back.
func (s *Service) failSignIn(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	_ = status // preserved for readability at call sites; the redirect carries the reason
	q := "?error=" + urlQueryEscape(code) + "&error_description=" + urlQueryEscape(message)
	http.Redirect(w, r, "/signin"+q, http.StatusFound)
}

func (s *Service) clearFlowCookies(w http.ResponseWriter, p *ssoProvider) {
	for _, name := range []string{p.stateCookie(), p.verifierCookie()} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/auth/" + p.key, MaxAge: -1,
			HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
		})
	}
}
