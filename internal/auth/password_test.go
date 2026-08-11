package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	const pw = "correct horse battery staple"

	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Errorf("hash = %q, want PHC argon2id format", hash)
	}
	if strings.Contains(hash, pw) {
		t.Fatal("hash contains the plaintext password")
	}

	if err := VerifyPassword(hash, pw); err != nil {
		t.Errorf("VerifyPassword with correct password: %v", err)
	}
	if err := VerifyPassword(hash, pw+"x"); !errors.Is(err, ErrMismatch) {
		t.Errorf("VerifyPassword with wrong password: err = %v, want ErrMismatch", err)
	}
	if err := VerifyPassword(hash, ""); !errors.Is(err, ErrMismatch) {
		t.Errorf("VerifyPassword with empty password: err = %v, want ErrMismatch", err)
	}
}

// Equal passwords must not produce equal hashes, or the database reveals which
// accounts share a password.
func TestHashesAreSalted(t *testing.T) {
	a, err := HashPassword("same")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	b, err := HashPassword("same")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if a == b {
		t.Error("two hashes of the same password are identical; salt is not random")
	}
	// Both must still verify.
	for _, h := range []string{a, b} {
		if err := VerifyPassword(h, "same"); err != nil {
			t.Errorf("VerifyPassword: %v", err)
		}
	}
}

func TestEmptyPasswordRejected(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Error("HashPassword(\"\") succeeded, want error")
	}
}

// A corrupt stored hash must be distinguishable from a wrong password: one is
// an operational fault worth alerting on, the other is a routine failed login.
func TestMalformedHashes(t *testing.T) {
	valid, err := HashPassword("pw")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	cases := map[string]string{
		"empty":            "",
		"not phc":          "plaintext",
		"too few fields":   "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA",
		"wrong algorithm":  "$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"wrong version":    "$argon2id$v=16$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"bad params":       "$argon2id$v=19$m=abc,t=2,p=1$c2FsdA$aGFzaA",
		"zero memory":      "$argon2id$v=19$m=0,t=2,p=1$c2FsdA$aGFzaA",
		"bad base64 salt":  "$argon2id$v=19$m=19456,t=2,p=1$!!!$aGFzaA",
		"empty hash field": "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$",
		"bcrypt":           "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy",
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			if err := VerifyPassword(encoded, "pw"); !errors.Is(err, ErrInvalidHash) {
				t.Errorf("VerifyPassword(%q) err = %v, want ErrInvalidHash", encoded, err)
			}
		})
	}

	if err := VerifyPassword(valid, "pw"); err != nil {
		t.Errorf("valid hash regressed: %v", err)
	}
}

func TestNeedsRehash(t *testing.T) {
	current, err := HashPassword("pw")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if NeedsRehash(current) {
		t.Error("a freshly created hash reports NeedsRehash")
	}

	// Produced under weaker parameters than the current settings.
	if !NeedsRehash("$argon2id$v=19$m=1024,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNo") {
		t.Error("a weaker hash does not report NeedsRehash")
	}
	// Unreadable records are treated as stale so a successful login repairs them.
	if !NeedsRehash("garbage") {
		t.Error("a malformed hash does not report NeedsRehash")
	}
}

// The parameters must stay at or above the OWASP argon2id floor. A well-meaning
// edit that lowers them for speed should fail here rather than silently weaken
// every password.
func TestParametersMeetMinimums(t *testing.T) {
	if argonMemory < 19*1024 {
		t.Errorf("argonMemory = %d KiB, want >= 19456", argonMemory)
	}
	if argonTime < 2 {
		t.Errorf("argonTime = %d, want >= 2", argonTime)
	}
	if argonKeyLen < 32 {
		t.Errorf("argonKeyLen = %d, want >= 32", argonKeyLen)
	}
	if argonSaltLen < 16 {
		t.Errorf("argonSaltLen = %d, want >= 16", argonSaltLen)
	}
}

func TestTokenGeneration(t *testing.T) {
	a, digestA, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	b, _, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if a == b {
		t.Error("NewToken returned the same token twice")
	}
	if len(a) < 40 {
		t.Errorf("token length = %d, want a 256-bit value", len(a))
	}
	if len(digestA) != 32 {
		t.Errorf("digest length = %d, want 32", len(digestA))
	}
	if string(digestA) == a {
		t.Error("digest equals the token; it is not hashed")
	}
	if got := TokenDigest(a); string(got) != string(digestA) {
		t.Error("TokenDigest disagrees with the digest returned by NewToken")
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !ConstantTimeEqual("abc", "abc") {
		t.Error("equal strings reported unequal")
	}
	for _, pair := range [][2]string{{"abc", "abd"}, {"abc", ""}, {"", "abc"}, {"abc", "abcd"}} {
		if ConstantTimeEqual(pair[0], pair[1]) {
			t.Errorf("ConstantTimeEqual(%q, %q) = true, want false", pair[0], pair[1])
		}
	}
	if !ConstantTimeEqual("", "") {
		t.Error("two empty strings reported unequal")
	}
}
