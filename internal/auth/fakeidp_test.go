//go:build integration

package auth_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeIdP is a minimal OIDC provider: a discovery document, a JWKS endpoint,
// and a token endpoint that mints RS256 id_tokens for whatever claims the test
// sets next.
//
// Testing against a real provider is not possible in CI, and mocking at the
// library boundary would skip exactly the parts worth testing — signature
// verification, issuer and audience checks, and claim handling. A real signed
// token through the real go-oidc verifier exercises all of it.
type fakeIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	claims map[string]any
	// issuer overrides the advertised issuer, so a test can model an Authentik
	// style issuer that ends with a trailing slash.
	issuer string
	// omitIDToken models a provider that returns no id_token at all.
	omitIDToken bool
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	idp := &fakeIdP{key: key}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		issuer := idp.srv.URL
		if idp.issuer != "" {
			issuer = idp.issuer
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                idp.srv.URL + "/authorize",
			"token_endpoint":                        idp.srv.URL + "/token",
			"jwks_uri":                              idp.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{"access_token": "at", "token_type": "Bearer"}
		if !idp.omitIDToken {
			body["id_token"] = idp.signToken(t)
		}
		_ = json.NewEncoder(w).Encode(body)
	})

	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

// setClaims installs the identity the next token exchange will assert.
func (idp *fakeIdP) setClaims(audience string, extra map[string]any) {
	claims := map[string]any{
		"iss": idp.issuerValue(),
		"aud": audience,
		"sub": "subject-1",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	for k, v := range extra {
		claims[k] = v
	}
	idp.claims = claims
}

func (idp *fakeIdP) issuerValue() string {
	if idp.issuer != "" {
		return idp.issuer
	}
	return idp.srv.URL
}

func (idp *fakeIdP) signToken(t *testing.T) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test", "typ": "JWT"})
	payload, _ := json.Marshal(idp.claims)
	signing := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}
