package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jonathanng/echo/internal/db/dbgen"
)

const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// touchInterval throttles the last_seen_at write. Updating on every request
// would turn each authenticated GET into a write transaction, which is a lot of
// churn for a field only used to show "last active".
const touchInterval = 5 * time.Minute

// Identity is the authenticated caller, attached to the request context by
// ResolveSession and read by handlers.
type Identity struct {
	UserID    uuid.UUID
	Email     string
	Role      string
	SessionID uuid.UUID
	CSRFToken string
}

func (i *Identity) IsAdmin() bool { return i != nil && i.Role == RoleAdmin }

// ctxKey is unexported so no other package can forge an identity; the only way
// in is WithIdentity, which only the session middleware calls.
type ctxKey struct{}

func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the authenticated caller, or nil when anonymous.
func FromContext(ctx context.Context) *Identity {
	id, _ := ctx.Value(ctxKey{}).(*Identity)
	return id
}

// ErrNoSession means the presented token is unknown, expired, or belongs to a
// disabled account — the three cases a caller handles identically.
var ErrNoSession = errors.New("auth: no valid session")

// ResolveSession exchanges a bearer token for an Identity, refreshing
// last_seen_at at most once per touchInterval.
func (s *Service) ResolveSession(ctx context.Context, token string) (*Identity, error) {
	row, err := s.q.GetSessionByTokenHash(ctx, TokenDigest(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoSession
		}
		return nil, err
	}

	if time.Since(row.Session.LastSeenAt) > touchInterval {
		if err := s.q.TouchSession(ctx, row.Session.ID); err != nil {
			// Not fatal: the session is valid, we merely failed to record use.
			s.log.Warn("touch session failed", "error", err)
		}
	}

	return &Identity{
		UserID:    row.User.ID,
		Email:     row.User.Email,
		Role:      string(row.User.Role),
		SessionID: row.Session.ID,
		CSRFToken: row.Session.CsrfToken,
	}, nil
}

// SignOut revokes a single session.
func (s *Service) SignOut(ctx context.Context, sessionID uuid.UUID) error {
	return s.q.DeleteSession(ctx, sessionID)
}

// User loads the full row for an identity, for endpoints that return a profile.
func (s *Service) User(ctx context.Context, id uuid.UUID) (dbgen.User, error) {
	return s.q.GetUser(ctx, id)
}
