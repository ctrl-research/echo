package auth

import "context"

// googleIssuer is overridable so tests can point discovery at a fake IdP.
// Empty in production.
var googleIssuer = "https://accounts.google.com"

func newGoogleProvider(ctx context.Context, opts Options, baseURL string) (*ssoProvider, error) {
	// Google always sets email_verified, so requiring it costs nothing and
	// closes the door on an unverified address being accepted as identity.
	return newSSOProvider(ctx, "google", "Google", googleIssuer,
		opts.GoogleClientID, opts.GoogleClientSecret, baseURL, true)
}

// SetGoogleIssuerForTest points Google discovery at a different issuer.
// Integration tests use it to substitute a fake identity provider; production
// never calls it.
func SetGoogleIssuerForTest(issuer string) { googleIssuer = issuer }
