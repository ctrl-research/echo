// Package config loads Echo's runtime configuration from the environment.
//
// Every setting is read from an ECHO_-prefixed variable so that the same
// binary is configurable identically under docker-compose and Kubernetes.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Addr is the host:port the HTTP server binds to.
	Addr string

	// DatabaseURL is a libpq-style connection string.
	DatabaseURL string

	// LibraryRoots are absolute paths scanned for audio files. The first
	// writable root receives promoted YouTube downloads.
	LibraryRoots []string

	// CacheDir holds derived, disposable data: transcodes, YouTube cache,
	// extracted cover art.
	CacheDir string

	YTCacheTTL             time.Duration
	YTCacheMaxBytes        int64
	TranscodeCacheMaxBytes int64

	// BaseURL is the instance's public URL. OAuth redirect URIs derive from
	// it, and cookies are marked Secure when it is https. No trailing slash.
	BaseURL string

	GoogleClientID     string
	GoogleClientSecret string

	// Generic OIDC provider (Authentik, Keycloak, Pocket ID, …). Issuer,
	// client id, and secret must be set together; OIDCName labels the button.
	OIDCIssuerURL    string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCName         string

	// LocalAuth enables email/password sign-in. Off by default.
	LocalAuth bool

	// AllowedEmails may create accounts after the first user exists.
	AllowedEmails []string

	// ScanWorkers is how many background job workers run.
	ScanWorkers int

	// ScanOnStart queues a full scan at startup. On by default: a library that
	// changed while the server was down should not need a manual nudge.
	ScanOnStart bool

	// SessionTTL is how long a session lasts before re-authentication.
	SessionTTL time.Duration

	// AdminEmail and AdminPassword bootstrap a local administrator, applied
	// only when the users table is empty and LocalAuth is on.
	AdminEmail    string
	AdminPassword string

	LogLevel string
}

// Load reads configuration from the environment, applying defaults for
// everything except DatabaseURL, which has no sane default.
func Load() (*Config, error) {
	c := &Config{
		Addr:                   env("ECHO_ADDR", ":8080"),
		DatabaseURL:            env("ECHO_DATABASE_URL", ""),
		CacheDir:               env("ECHO_CACHE_DIR", "./cache"),
		LogLevel:               env("ECHO_LOG_LEVEL", "info"),
		LibraryRoots:           splitList(env("ECHO_LIBRARY_ROOTS", "")),
		YTCacheTTL:             48 * time.Hour,
		YTCacheMaxBytes:        5 << 30,
		TranscodeCacheMaxBytes: 10 << 30,
		SessionTTL:             30 * 24 * time.Hour,
		ScanWorkers:            4,
		ScanOnStart:            env("ECHO_SCAN_ON_START", "true") == "true",
		BaseURL:                strings.TrimSuffix(env("ECHO_BASE_URL", "http://localhost:8080"), "/"),
		GoogleClientID:         env("ECHO_GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret:     env("ECHO_GOOGLE_CLIENT_SECRET", ""),
		// The OIDC issuer is an exact-match identifier; copy it verbatim from
		// the provider's discovery document.
		OIDCIssuerURL:    env("ECHO_OIDC_ISSUER_URL", ""),
		OIDCClientID:     env("ECHO_OIDC_CLIENT_ID", ""),
		OIDCClientSecret: env("ECHO_OIDC_CLIENT_SECRET", ""),
		OIDCName:         env("ECHO_OIDC_NAME", ""),
		LocalAuth:        env("ECHO_LOCAL_AUTH", "") == "true",
		AllowedEmails:    splitList(env("ECHO_ALLOWED_EMAILS", "")),
		AdminEmail:       env("ECHO_ADMIN_EMAIL", ""),
		AdminPassword:    env("ECHO_ADMIN_PASSWORD", ""),
	}

	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("ECHO_DATABASE_URL is required"))
	}

	// Each provider needs its whole credential set; a partial one means a
	// button that appears and then fails, so refuse to start instead.
	if (c.GoogleClientID == "") != (c.GoogleClientSecret == "") {
		errs = append(errs, errors.New(
			"ECHO_GOOGLE_CLIENT_ID and ECHO_GOOGLE_CLIENT_SECRET must be set together"))
	}
	oidcSet := 0
	for _, v := range []string{c.OIDCIssuerURL, c.OIDCClientID, c.OIDCClientSecret} {
		if v != "" {
			oidcSet++
		}
	}
	if oidcSet != 0 && oidcSet != 3 {
		errs = append(errs, errors.New(
			"ECHO_OIDC_ISSUER_URL, ECHO_OIDC_CLIENT_ID, and ECHO_OIDC_CLIENT_SECRET must be set together"))
	}
	if v := os.Getenv("ECHO_SCAN_WORKERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			errs = append(errs, errors.New("ECHO_SCAN_WORKERS must be a positive integer"))
		} else {
			c.ScanWorkers = n
		}
	}
	if v := os.Getenv("ECHO_SESSION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("ECHO_SESSION_TTL: %w", err))
		} else if d <= 0 {
			errs = append(errs, errors.New("ECHO_SESSION_TTL must be positive"))
		} else {
			c.SessionTTL = d
		}
	}
	// Half a bootstrap is always a mistake, and failing at startup is far
	// kinder than silently never creating the account.
	if (c.AdminEmail == "") != (c.AdminPassword == "") {
		errs = append(errs, errors.New(
			"ECHO_ADMIN_EMAIL and ECHO_ADMIN_PASSWORD must be set together"))
	}
	if c.AdminEmail != "" && !c.LocalAuth {
		errs = append(errs, errors.New(
			"ECHO_ADMIN_EMAIL requires ECHO_LOCAL_AUTH=true; without password "+
				"sign-in the bootstrap account could never be used"))
	}

	if v := os.Getenv("ECHO_YT_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("ECHO_YT_CACHE_TTL: %w", err))
		} else {
			c.YTCacheTTL = d
		}
	}
	if err := envBytes("ECHO_YT_CACHE_MAX_BYTES", &c.YTCacheMaxBytes); err != nil {
		errs = append(errs, err)
	}
	if err := envBytes("ECHO_TRANSCODE_CACHE_MAX_BYTES", &c.TranscodeCacheMaxBytes); err != nil {
		errs = append(errs, err)
	}

	return c, errors.Join(errs...)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envBytes(key string, dst *int64) error {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	n, err := ParseBytes(v)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	*dst = n
	return nil
}

// ParseBytes parses a human-written size such as "5GB", "512MiB", or "1024".
// Both SI and IEC suffixes are accepted and both are interpreted as binary
// multiples, which is what people mean when sizing a disk cache.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}

	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, unit := s[:i], strings.ToUpper(strings.TrimSpace(s[i:]))
	if num == "" {
		return 0, fmt.Errorf("no numeric part in %q", s)
	}

	// Normalise "GB" and "GiB" alike down to "G"; a bare "B" or no unit to "".
	base := strings.TrimSuffix(unit, "B")
	base = strings.TrimSuffix(base, "I")

	var mult int64
	switch base {
	case "":
		mult = 1
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("unknown unit %q", unit)
	}

	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	if f < 0 {
		return 0, fmt.Errorf("negative size %q", s)
	}
	return int64(f * float64(mult)), nil
}
