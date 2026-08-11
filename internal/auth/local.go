package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jonathanng/echo/internal/db/dbgen"
)

// ErrLocalAuthDisabled is returned when password sign-in is attempted on an
// instance that has not enabled it.
var ErrLocalAuthDisabled = errors.New("auth: local sign-in is disabled")

// ErrInvalidCredentials covers every password sign-in failure: unknown email,
// wrong password, an account with no password set, and a disabled account.
// They are deliberately indistinguishable to the caller, so that an
// unauthenticated attacker cannot enumerate accounts.
var ErrInvalidCredentials = errors.New("auth: invalid credentials")

// Authenticate verifies an email/password pair and returns the user.
func (s *Service) Authenticate(ctx context.Context, email, password string) (dbgen.User, error) {
	if !s.localAuth {
		return dbgen.User{}, ErrLocalAuthDisabled
	}
	email = strings.ToLower(strings.TrimSpace(email))

	user, err := s.q.GetUserByEmail(ctx, email)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return dbgen.User{}, fmt.Errorf("lookup user: %w", err)
		}
		// Spend the same time as a real verification, so response latency does
		// not reveal whether the address is registered.
		DummyVerify(password)
		return dbgen.User{}, ErrInvalidCredentials
	}

	if user.PasswordHash == nil {
		// An SSO-only account. Same dummy work, same error: revealing that the
		// address exists but signs in another way is still enumeration.
		DummyVerify(password)
		return dbgen.User{}, ErrInvalidCredentials
	}

	if err := VerifyPassword(*user.PasswordHash, password); err != nil {
		if errors.Is(err, ErrInvalidHash) {
			// A corrupt stored hash is an operational fault, not a bad password.
			s.log.Error("stored password hash is malformed", "user", user.Email, "error", err)
		}
		return dbgen.User{}, ErrInvalidCredentials
	}

	// Checked after verification, again to keep timing uniform.
	if user.DisabledAt.Valid {
		return dbgen.User{}, ErrInvalidCredentials
	}

	// The plaintext is only available here, so this is the one opportunity to
	// upgrade a hash produced under weaker parameters.
	if NeedsRehash(*user.PasswordHash) {
		if rehashed, err := HashPassword(password); err == nil {
			if _, err := s.q.UpdateUser(ctx, dbgen.UpdateUserParams{
				ID: user.ID, PasswordHash: &rehashed,
			}); err != nil {
				s.log.Warn("password rehash failed", "error", err)
			}
		}
	}
	return user, nil
}

// SetPassword replaces a user's password and revokes every session they hold,
// including the caller's. Changing a password after a suspected compromise is
// pointless if existing sessions survive it.
func (s *Service) SetPassword(ctx context.Context, userID uuid.UUID, newPassword string) error {
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	if _, err := s.q.UpdateUser(ctx, dbgen.UpdateUserParams{
		ID: userID, PasswordHash: &hash,
	}); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if err := s.q.DeleteSessionsForUser(ctx, userID); err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	return nil
}
