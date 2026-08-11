package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

const (
	// SessionCookie carries the bearer token. HttpOnly, so script cannot read
	// it; this is what makes <audio src> and service-worker range requests
	// authenticate without any client-side header plumbing.
	SessionCookie = "echo_session"

	// CSRFCookie carries the double-submit token. Deliberately NOT HttpOnly:
	// the client reads it and echoes it back in CSRFHeader.
	CSRFCookie = "echo_csrf"

	// CSRFHeader is where the client returns the CSRF token.
	CSRFHeader = "X-CSRF-Token"

	tokenBytes = 32 // 256 bits
)

// NewToken returns a URL-safe random token and its SHA-256 digest.
//
// Only the digest is stored. A 256-bit CSPRNG value has no structure to attack
// offline, so a fast hash is the right choice here — unlike a password, there
// is no low-entropy preimage to guess. Slow-hashing it would add latency to
// every authenticated request for no security gain.
func NewToken() (token string, digest []byte, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("auth: read token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(token))
	return token, sum[:], nil
}

// TokenDigest hashes a presented token for lookup.
func TokenDigest(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// NewCSRFToken returns a random token for the double-submit cookie pattern.
func NewCSRFToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: read csrf token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ConstantTimeEqual compares two tokens without leaking their contents through
// timing. Used for CSRF, where both values arrive from the request.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
