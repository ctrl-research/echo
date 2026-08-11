package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonathanng/echo/internal/db/dbgen"
)

// BootstrapLocalAdmin creates a local administrator when the users table is
// empty and local sign-in is enabled.
//
// With SSO this is usually unnecessary — the first person to sign in claims the
// instance and becomes administrator. It exists for two cases: bringing an
// instance up before an identity provider is configured, and keeping a
// break-glass account for when the provider is unreachable.
//
// The guard is "no users at all", not "no admin". Recreating an account an
// operator deliberately deleted would be surprising, and an environment that
// still carries the variables would resurrect it on every restart. An empty
// table is unambiguous: nobody can sign in, so there is nothing to override.
func BootstrapLocalAdmin(ctx context.Context, q *dbgen.Queries, log LoggerLike, email, password string) error {
	if email == "" || password == "" {
		return nil
	}

	count, err := q.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if count > 0 {
		return nil
	}

	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash bootstrap password: %w", err)
	}

	user, err := q.CreateUser(ctx, dbgen.CreateUserParams{
		Email:        strings.ToLower(strings.TrimSpace(email)),
		DisplayName:  "Administrator",
		PasswordHash: &hash,
		Role:         dbgen.UserRoleAdmin,
	})
	if err != nil {
		return fmt.Errorf("create bootstrap admin: %w", err)
	}

	log.Info("bootstrap administrator created", "email", user.Email)
	return nil
}

// LoggerLike is the sliver of *slog.Logger this package needs, so tests can
// pass a no-op without constructing a handler.
type LoggerLike interface {
	Info(msg string, args ...any)
}
