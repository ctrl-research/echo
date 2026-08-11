// Package auth implements password hashing, session tokens, and the request
// context plumbing that carries an authenticated user to handlers.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2 parameters. These follow OWASP's second recommended argon2id
// configuration (19 MiB, t=2, p=1) rather than a heavier one.
//
// The tradeoff is deliberate: login is unauthenticated, so every attempt costs
// the server its memory cost up front. A 64 MiB setting turns a handful of
// concurrent login attempts into memory pressure on a small home server. 19
// MiB with t=2 is still far beyond feasible offline cracking for any
// reasonable password, and it keeps the denial-of-service surface small.
const (
	argonMemory  uint32 = 19 * 1024 // KiB
	argonTime    uint32 = 2
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// argonParallelism is capped at 4 so that a machine with many cores does not
// produce hashes that a smaller machine cannot verify at the same cost.
var argonParallelism = uint8(min(runtime.NumCPU(), 4))

var (
	ErrInvalidHash = errors.New("auth: malformed password hash")
	ErrMismatch    = errors.New("auth: password does not match")
)

// HashPassword returns a PHC-format argon2id string:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
//
// Encoding the parameters alongside the digest means they can be raised later
// without invalidating stored passwords; see NeedsRehash.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("auth: empty password")
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}

	p := argonParallelism
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, p, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, p,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches encoded. It returns
// ErrMismatch for a wrong password and ErrInvalidHash for a corrupt record, so
// callers can log the two differently — a malformed hash is an operational
// problem, not a failed login.
func VerifyPassword(encoded, password string) error {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return err
	}

	got := argon2.IDKey([]byte(password), salt,
		params.time, params.memory, params.parallelism, uint32(len(want)))

	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// NeedsRehash reports whether a stored hash was produced with weaker
// parameters than the current settings. Callers rehash on successful login,
// which is the only moment the plaintext is available.
func NeedsRehash(encoded string) bool {
	params, _, _, err := decodeHash(encoded)
	if err != nil {
		// Unreadable hashes cannot be verified against anyway; treating them as
		// stale means a successful login repairs the record.
		return true
	}
	return params.memory < argonMemory ||
		params.time < argonTime ||
		params.parallelism < argonParallelism
}

// DummyVerify performs a hash computation with the standard parameters and
// discards the result.
//
// Sign-in calls this when the address does not exist, so that a request for an
// unknown account costs the same wall-clock time as one for a real account.
// Without it, response latency reveals which addresses are registered.
func DummyVerify(password string) {
	_ = argon2.IDKey([]byte(password), dummySalt,
		argonTime, argonMemory, argonParallelism, argonKeyLen)
}

var dummySalt = make([]byte, argonSaltLen)

type argonParams struct {
	memory      uint32
	time        uint32
	parallelism uint8
}

func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	var p argonParams

	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 || parts[0] != "" {
		return p, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return p, nil, nil, fmt.Errorf("%w: algorithm %q", ErrInvalidHash, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return p, nil, nil, fmt.Errorf("%w: version %d", ErrInvalidHash, version)
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.parallelism); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if p.memory == 0 || p.time == 0 || p.parallelism == 0 {
		return p, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return p, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return p, nil, nil, ErrInvalidHash
	}

	return p, salt, key, nil
}
