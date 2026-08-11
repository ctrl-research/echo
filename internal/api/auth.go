package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/jonathanng/echo/internal/auth"
	"github.com/jonathanng/echo/internal/db/dbgen"
)

// UserDTO is the API representation of a user. Deliberately not the dbgen row:
// password_hash and provider subjects must never be serialisable, and pinning
// the wire shape here keeps a schema change from silently altering the API.
type UserDTO struct {
	ID          string    `json:"id" format:"uuid" doc:"Stable identifier"`
	Email       string    `json:"email" format:"email"`
	DisplayName string    `json:"displayName"`
	AvatarURL   string    `json:"avatarUrl,omitempty" required:"false"`
	Role        string    `json:"role" enum:"admin,user"`
	CreatedAt   time.Time `json:"createdAt"`
	Disabled    bool      `json:"disabled" doc:"Disabled accounts cannot sign in"`
	// Providers lists the sign-in methods linked to this account.
	Providers []string `json:"providers" doc:"Any of google, oidc, local"`
}

func toUserDTO(u dbgen.User) UserDTO {
	dto := UserDTO{
		ID:          u.ID.String(),
		Email:       u.Email,
		DisplayName: u.DisplayName,
		Role:        string(u.Role),
		CreatedAt:   u.CreatedAt,
		Disabled:    u.DisabledAt.Valid,
		Providers:   []string{},
	}
	if u.AvatarUrl != nil {
		dto.AvatarURL = *u.AvatarUrl
	}
	if u.GoogleSub != nil {
		dto.Providers = append(dto.Providers, "google")
	}
	if u.OidcSub != nil {
		dto.Providers = append(dto.Providers, "oidc")
	}
	if u.PasswordHash != nil {
		dto.Providers = append(dto.Providers, "local")
	}
	return dto
}

func (s *Server) registerAuth() {
	huma.Register(s.API, huma.Operation{
		OperationID: "providers",
		Method:      http.MethodGet,
		Path:        "/auth/providers",
		Summary:     "Available sign-in methods",
		Description: "Tells the client which sign-in buttons to offer. Unauthenticated.",
		Tags:        []string{"auth"},
	}, s.handleProviders)

	huma.Register(s.API, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        "/auth/login",
		Summary:     "Sign in with email and password",
		Description: "Only available when local sign-in is enabled; otherwise returns 404.",
		Tags:        []string{"auth"},
	}, s.handleLogin)

	huma.Register(s.API, huma.Operation{
		OperationID: "logout",
		Method:      http.MethodPost,
		Path:        "/auth/logout",
		Summary:     "Sign out",
		Description: "Revokes the current session.",
		Tags:        []string{"auth"},
	}, s.handleLogout)

	huma.Register(s.API, huma.Operation{
		OperationID: "me",
		Method:      http.MethodGet,
		Path:        "/auth/me",
		Summary:     "Current user",
		Description: "Returns the signed-in user, or 401 when anonymous.",
		Tags:        []string{"auth"},
	}, s.handleMe)

	huma.Register(s.API, huma.Operation{
		OperationID: "changePassword",
		Method:      http.MethodPost,
		Path:        "/auth/password",
		Summary:     "Change own password",
		Description: "Local accounts only. Requires the current password and revokes all sessions.",
		Tags:        []string{"auth"},
	}, s.handleChangePassword)
}

// ---- providers -------------------------------------------------------------

type ProvidersOutput struct {
	Body struct {
		Providers []auth.Provider `json:"providers"`
	}
}

func (s *Server) handleProviders(ctx context.Context, _ *struct{}) (*ProvidersOutput, error) {
	out := &ProvidersOutput{}
	out.Body.Providers = s.deps.Auth.Providers()
	return out, nil
}

// ---- local sign-in ---------------------------------------------------------

type LoginInput struct {
	UserAgent string `header:"User-Agent"`
	Body      struct {
		Email    string `json:"email" format:"email" minLength:"3" maxLength:"320"`
		Password string `json:"password" minLength:"1" maxLength:"1024"`
	}
}

type LoginOutput struct {
	SetCookie []http.Cookie `header:"Set-Cookie"`
	Body      UserDTO
}

func (s *Server) handleLogin(ctx context.Context, in *LoginInput) (*LoginOutput, error) {
	user, err := s.deps.Auth.Authenticate(ctx, in.Body.Email, in.Body.Password)
	switch {
	case errors.Is(err, auth.ErrLocalAuthDisabled):
		// 404 rather than 403: on an SSO-only instance this endpoint may as
		// well not exist, and saying so invites no further probing.
		return nil, huma.Error404NotFound("Password sign-in is not enabled on this instance")
	case errors.Is(err, auth.ErrInvalidCredentials):
		return nil, huma.Error401Unauthorized("Invalid email or password")
	case err != nil:
		s.deps.Log.Error("login failed", "error", err)
		return nil, huma.Error500InternalServerError("Sign-in failed")
	}

	cookies, err := s.issueSession(ctx, user, in.UserAgent)
	if err != nil {
		s.deps.Log.Error("login: create session failed", "error", err)
		return nil, huma.Error500InternalServerError("Sign-in failed")
	}
	s.deps.Log.Info("sign-in", "provider", "local", "user", user.Email, "role", user.Role)
	return &LoginOutput{SetCookie: cookies, Body: toUserDTO(user)}, nil
}

// issueSession adapts Service.SignIn, which writes to an http.ResponseWriter,
// to huma's model of returning cookies as response values.
func (s *Server) issueSession(ctx context.Context, user dbgen.User, userAgent string) ([]http.Cookie, error) {
	rec := &cookieRecorder{header: http.Header{}}
	if err := s.deps.Auth.SignIn(ctx, rec, user, userAgent); err != nil {
		return nil, err
	}
	return rec.cookies(), nil
}

// cookieRecorder captures Set-Cookie headers without a real response.
type cookieRecorder struct{ header http.Header }

func (c *cookieRecorder) Header() http.Header       { return c.header }
func (c *cookieRecorder) Write([]byte) (int, error) { return 0, nil }
func (c *cookieRecorder) WriteHeader(int)           {}

func (c *cookieRecorder) cookies() []http.Cookie {
	resp := http.Response{Header: c.header}
	out := make([]http.Cookie, 0, 2)
	for _, ck := range resp.Cookies() {
		out = append(out, *ck)
	}
	return out
}

// ---- logout ----------------------------------------------------------------

type LogoutOutput struct {
	SetCookie []http.Cookie `header:"Set-Cookie"`
	Status    int
}

func (s *Server) handleLogout(ctx context.Context, _ *struct{}) (*LogoutOutput, error) {
	identity := auth.FromContext(ctx)
	if identity == nil {
		// Signing out while already signed out is not an error; an identical
		// response keeps the client's cleanup path simple.
		return &LogoutOutput{SetCookie: s.deps.Auth.ExpiredCookies(), Status: http.StatusNoContent}, nil
	}
	if err := s.deps.Auth.SignOut(ctx, identity.SessionID); err != nil {
		s.deps.Log.Error("logout: delete session failed", "error", err)
		return nil, huma.Error500InternalServerError("Sign-out failed")
	}
	return &LogoutOutput{SetCookie: s.deps.Auth.ExpiredCookies(), Status: http.StatusNoContent}, nil
}

// ---- me --------------------------------------------------------------------

type MeOutput struct {
	Body UserDTO
}

func (s *Server) handleMe(ctx context.Context, _ *struct{}) (*MeOutput, error) {
	identity := auth.FromContext(ctx)
	if identity == nil {
		return nil, huma.Error401Unauthorized("Not signed in")
	}
	user, err := s.deps.Auth.User(ctx, identity.UserID)
	if err != nil {
		s.deps.Log.Error("me: user lookup failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not load user")
	}
	return &MeOutput{Body: toUserDTO(user)}, nil
}

// ---- change own password ---------------------------------------------------

type ChangePasswordInput struct {
	UserAgent string `header:"User-Agent"`
	Body      struct {
		CurrentPassword string `json:"currentPassword" minLength:"1" maxLength:"1024"`
		NewPassword     string `json:"newPassword" minLength:"8" maxLength:"1024"`
	}
}

func (s *Server) handleChangePassword(ctx context.Context, in *ChangePasswordInput) (*LoginOutput, error) {
	identity := auth.FromContext(ctx)
	if identity == nil {
		return nil, huma.Error401Unauthorized("Not signed in")
	}
	if !s.deps.Auth.LocalAuthEnabled() {
		return nil, huma.Error404NotFound("Password sign-in is not enabled on this instance")
	}

	user, err := s.deps.Auth.User(ctx, identity.UserID)
	if err != nil {
		s.deps.Log.Error("change password: user lookup failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not change password")
	}
	if _, err := s.deps.Auth.Authenticate(ctx, user.Email, in.Body.CurrentPassword); err != nil {
		return nil, huma.Error403Forbidden("Current password is incorrect")
	}

	// SetPassword revokes every session, including this one — that is the
	// point of changing a password. A fresh session is issued below so the
	// caller is not signed out of the browser they are using.
	if err := s.deps.Auth.SetPassword(ctx, user.ID, in.Body.NewPassword); err != nil {
		s.deps.Log.Error("change password failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not change password")
	}

	cookies, err := s.issueSession(ctx, user, in.UserAgent)
	if err != nil {
		s.deps.Log.Error("change password: new session failed", "error", err)
		return nil, huma.Error500InternalServerError("Could not change password")
	}
	s.deps.Log.Info("password changed", "user", user.Email)
	return &LoginOutput{SetCookie: cookies, Body: toUserDTO(user)}, nil
}
