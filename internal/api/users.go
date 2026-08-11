package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jonathanng/echo/internal/auth"
	"github.com/jonathanng/echo/internal/db/dbgen"
)

// pgUniqueViolation is the SQLSTATE for a duplicate key. Checking the code is
// how a conflicting email becomes a 409 rather than a 500.
const pgUniqueViolation = "23505"

func (s *Server) registerUsers() {
	huma.Register(s.API, huma.Operation{
		OperationID: "listUsers", Method: http.MethodGet, Path: "/admin/users",
		Summary: "List users", Tags: []string{"admin"},
	}, s.handleListUsers)

	huma.Register(s.API, huma.Operation{
		OperationID: "createUser", Method: http.MethodPost, Path: "/admin/users",
		Summary: "Create a user", DefaultStatus: http.StatusCreated, Tags: []string{"admin"},
	}, s.handleCreateUser)

	huma.Register(s.API, huma.Operation{
		OperationID: "getUser", Method: http.MethodGet, Path: "/admin/users/{id}",
		Summary: "Get a user", Tags: []string{"admin"},
	}, s.handleGetUser)

	huma.Register(s.API, huma.Operation{
		OperationID: "updateUser", Method: http.MethodPatch, Path: "/admin/users/{id}",
		Summary: "Update a user", Tags: []string{"admin"},
	}, s.handleUpdateUser)

	huma.Register(s.API, huma.Operation{
		OperationID: "deleteUser", Method: http.MethodDelete, Path: "/admin/users/{id}",
		Summary: "Delete a user", DefaultStatus: http.StatusNoContent, Tags: []string{"admin"},
	}, s.handleDeleteUser)
}

type ListUsersOutput struct {
	Body struct {
		Users []UserDTO `json:"users"`
	}
}

func (s *Server) handleListUsers(ctx context.Context, _ *struct{}) (*ListUsersOutput, error) {
	rows, err := s.queries.ListUsers(ctx)
	if err != nil {
		s.deps.Log.Error("list users failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not list users")
	}
	out := &ListUsersOutput{}
	out.Body.Users = make([]UserDTO, 0, len(rows))
	for _, u := range rows {
		out.Body.Users = append(out.Body.Users, toUserDTO(u))
	}
	return out, nil
}

// CreateUserInput creates a *local* account. Accounts normally arrive through
// SSO — the first person to sign in claims the instance, and everyone after
// must be on ECHO_ALLOWED_EMAILS — so this exists for instances with password
// sign-in enabled, and for seeding a break-glass administrator.
type CreateUserInput struct {
	Body struct {
		Email       string `json:"email" format:"email" minLength:"3" maxLength:"320" doc:"Case-insensitive and unique"`
		Password    string `json:"password" minLength:"8" maxLength:"1024"`
		DisplayName string `json:"displayName,omitempty" maxLength:"128" required:"false"`
		Role        string `json:"role,omitempty" enum:"admin,user" default:"user" required:"false"`
	}
}

type UserOutput struct {
	Body UserDTO
}

func (s *Server) handleCreateUser(ctx context.Context, in *CreateUserInput) (*UserOutput, error) {
	if !s.deps.Auth.LocalAuthEnabled() {
		return nil, huma.Error409Conflict(
			"Password sign-in is disabled, so accounts with a password cannot be created. " +
				"Add the address to ECHO_ALLOWED_EMAILS and have them sign in instead.")
	}

	role := in.Body.Role
	if role == "" {
		role = auth.RoleUser
	}

	hash, err := auth.HashPassword(in.Body.Password)
	if err != nil {
		return nil, huma.Error500InternalServerError("Could not create user")
	}

	user, err := s.queries.CreateUser(ctx, dbgen.CreateUserParams{
		Email:        strings.ToLower(strings.TrimSpace(in.Body.Email)),
		DisplayName:  in.Body.DisplayName,
		PasswordHash: &hash,
		Role:         dbgen.UserRole(role),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, huma.Error409Conflict("An account with that email already exists")
		}
		s.deps.Log.Error("create user failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not create user")
	}
	s.deps.Log.Info("user created", "user", user.Email, "role", user.Role)
	return &UserOutput{Body: toUserDTO(user)}, nil
}

type UserIDInput struct {
	ID string `path:"id" format:"uuid"`
}

func (s *Server) handleGetUser(ctx context.Context, in *UserIDInput) (*UserOutput, error) {
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed user id")
	}
	user, err := s.queries.GetUser(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("No such user")
		}
		s.deps.Log.Error("get user failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not load user")
	}
	return &UserOutput{Body: toUserDTO(user)}, nil
}

type UpdateUserInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		// Pointers so that "absent" and "explicitly set" stay distinguishable;
		// a PATCH must be able to change role without touching disabled.
		Password *string `json:"password,omitempty" minLength:"8" maxLength:"1024"`
		Role     *string `json:"role,omitempty" enum:"admin,user"`
		Disabled *bool   `json:"disabled,omitempty"`
	}
}

func (s *Server) handleUpdateUser(ctx context.Context, in *UpdateUserInput) (*UserOutput, error) {
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed user id")
	}
	target, err := s.queries.GetUser(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("No such user")
		}
		return nil, huma.Error500InternalServerError("Could not load user")
	}

	// Demoting or disabling the last administrator would lock everyone out of
	// user management permanently, with no recovery path short of editing the
	// database by hand.
	losingAdmin := target.Role == dbgen.UserRoleAdmin &&
		((in.Body.Role != nil && *in.Body.Role != auth.RoleAdmin) ||
			(in.Body.Disabled != nil && *in.Body.Disabled))
	if losingAdmin && !target.DisabledAt.Valid {
		if err := s.assertNotLastAdmin(ctx); err != nil {
			return nil, err
		}
	}

	params := dbgen.UpdateUserParams{ID: id}
	if in.Body.Password != nil {
		hash, err := auth.HashPassword(*in.Body.Password)
		if err != nil {
			return nil, huma.Error500InternalServerError("Could not update user")
		}
		params.PasswordHash = &hash
	}
	if in.Body.Role != nil {
		role := dbgen.UserRole(*in.Body.Role)
		params.Role = &role
	}
	if in.Body.Disabled != nil {
		params.SetDisabled = true
		if *in.Body.Disabled {
			params.DisabledAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
		}
	}

	user, err := s.queries.UpdateUser(ctx, params)
	if err != nil {
		s.deps.Log.Error("update user failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not update user")
	}

	// Any of these three changes should end the target's existing sessions:
	// a new password, lost privileges, or a disabled account.
	if in.Body.Password != nil || in.Body.Role != nil || in.Body.Disabled != nil {
		if err := s.queries.DeleteSessionsForUser(ctx, id); err != nil {
			s.deps.Log.Error("update user: session revocation failed", "error", err)
		}
	}
	s.deps.Log.Info("user updated", "user", user.Email)
	return &UserOutput{Body: toUserDTO(user)}, nil
}

type DeleteUserOutput struct {
	Status int
}

func (s *Server) handleDeleteUser(ctx context.Context, in *UserIDInput) (*DeleteUserOutput, error) {
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Malformed user id")
	}
	target, err := s.queries.GetUser(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("No such user")
		}
		return nil, huma.Error500InternalServerError("Could not load user")
	}
	if target.Role == dbgen.UserRoleAdmin && !target.DisabledAt.Valid {
		if err := s.assertNotLastAdmin(ctx); err != nil {
			return nil, err
		}
	}

	n, err := s.queries.DeleteUser(ctx, id)
	if err != nil {
		s.deps.Log.Error("delete user failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not delete user")
	}
	if n == 0 {
		return nil, huma.Error404NotFound("No such user")
	}
	s.deps.Log.Info("user deleted", "user", target.Email)
	return &DeleteUserOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) assertNotLastAdmin(ctx context.Context) error {
	count, err := s.queries.CountActiveAdmins(ctx)
	if err != nil {
		s.deps.Log.Error("count admins failed", "error", err)
		return huma.Error500InternalServerError("Could not verify admin count")
	}
	if count <= 1 {
		return huma.Error409Conflict(
			"This is the only active administrator; promote another account first")
	}
	return nil
}
