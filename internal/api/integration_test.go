//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jonathanng/echo/internal/api"
	"github.com/jonathanng/echo/internal/db"
)

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := newRawDB(t)

	if err := db.Migrate(ctx, pool, discardLogger()); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	// A second run models a rolling deploy where two replicas start at once;
	// it must be a no-op rather than an error.
	if err := db.Migrate(ctx, pool, discardLogger()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	for _, ext := range []string{"citext", "pg_trgm", "unaccent"} {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)`, ext,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("query pg_extension: %v", err)
		}
		if !exists {
			t.Errorf("extension %q not installed", ext)
		}
	}
}

// immutable_unaccent must be usable in a generated column, which is the whole
// reason it exists. Postgres rejects non-IMMUTABLE functions there, so this
// asserts the volatility marking rather than just the folding behaviour.
func TestImmutableUnaccentWorksInGeneratedColumn(t *testing.T) {
	ctx := context.Background()
	pool := newTestDB(t)

	_, err := pool.Exec(ctx, `
		CREATE TABLE gen_probe (
			raw  text NOT NULL,
			tsv  tsvector GENERATED ALWAYS AS (
				to_tsvector('simple', immutable_unaccent(raw))
			) STORED
		)`)
	if err != nil {
		t.Fatalf("create generated column: %v", err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO gen_probe (raw) VALUES ('Björk')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var matches bool
	err = pool.QueryRow(ctx,
		`SELECT tsv @@ to_tsquery('simple', 'bjork') FROM gen_probe`,
	).Scan(&matches)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !matches {
		t.Error(`"bjork" did not match indexed "Björk"; unaccent folding is not applied`)
	}
}

// uuidv7() is a PostgreSQL 18 built-in. The schema depends on it for every
// primary key, so an older server must fail loudly here.
func TestUUIDv7Available(t *testing.T) {
	ctx := context.Background()
	pool := newTestDB(t)

	var a, b string
	if err := pool.QueryRow(ctx, `SELECT uuidv7()::text, uuidv7()::text`).Scan(&a, &b); err != nil {
		t.Fatalf("uuidv7() unavailable — PostgreSQL 18+ is required: %v", err)
	}
	if a == b {
		t.Error("uuidv7() returned identical values")
	}
	// Time-ordered generation is the reason for choosing v7 over v4; without
	// it the index-locality argument in docs/design.md does not hold.
	if a >= b {
		t.Errorf("uuidv7() not monotonic: %q >= %q", a, b)
	}
}

// Emails are citext, so case variants must collide rather than creating a
// second account. Identity providers disagree about case normalisation, so
// without this the same person could end up with two accounts.
func TestEmailsAreCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	pool := newTestDB(t)

	_, err := pool.Exec(ctx,
		`INSERT INTO users (email, password_hash) VALUES ('Jonathan@Example.com', 'x')`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (email, password_hash) VALUES ('jonathan@example.com', 'x')`); err == nil {
		t.Error("inserting a case variant succeeded; email uniqueness is case-sensitive")
	}

	var found string
	if err := pool.QueryRow(ctx,
		`SELECT email FROM users WHERE email = 'JONATHAN@EXAMPLE.COM'`).Scan(&found); err != nil {
		t.Errorf("case-insensitive lookup failed: %v", err)
	}
}

// Every account must have some way to sign in. An account with no credential
// is unreachable, so it is always a bug rather than a state worth allowing.
func TestUserWithoutCredentialIsRejected(t *testing.T) {
	ctx := context.Background()
	pool := newTestDB(t)

	_, err := pool.Exec(ctx, `INSERT INTO users (email) VALUES ('nobody@example.com')`)
	if err == nil {
		t.Fatal("inserted a user with no credential; the CHECK constraint is missing")
	}

	// Any one credential is enough.
	for i, col := range []string{"google_sub", "oidc_sub", "password_hash"} {
		_, err := pool.Exec(ctx,
			`INSERT INTO users (email, `+col+`) VALUES ($1, 'value')`,
			fmt.Sprintf("user%d@example.com", i))
		if err != nil {
			t.Errorf("insert with %s alone failed: %v", col, err)
		}
	}
}

// Both subject columns are unique: two accounts must never claim the same
// provider identity.
func TestProviderSubjectsAreUnique(t *testing.T) {
	ctx := context.Background()
	pool := newTestDB(t)

	for _, col := range []string{"google_sub", "oidc_sub"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (email, `+col+`) VALUES ($1, 'shared-subject')`,
			"first-"+col+"@example.com"); err != nil {
			t.Fatalf("first insert for %s: %v", col, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (email, `+col+`) VALUES ($1, 'shared-subject')`,
			"second-"+col+"@example.com"); err == nil {
			t.Errorf("two accounts share the same %s", col)
		}
	}
}

// Deleting a user must take their sessions with them rather than leaving rows
// that point at a missing account.
func TestSessionsCascadeOnUserDelete(t *testing.T) {
	ctx := context.Background()
	pool := newTestDB(t)

	var userID string
	err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ('gone@example.com', 'x') RETURNING id`,
	).Scan(&userID)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO sessions (user_id, token_hash, csrf_token, expires_at)
		 VALUES ($1, '\x00'::bytea, 'csrf', now() + interval '1 day')`, userID)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&remaining); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d sessions survived the user deletion", remaining)
	}
}

func TestHealthEndpoint(t *testing.T) {
	pool := newTestDB(t)

	srv := api.New(api.Deps{Pool: pool, Log: discardLogger()})
	rec := httptest.NewRecorder()
	srv.Router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}

	var body struct {
		Status   string `json:"status"`
		Version  string `json:"version"`
		Database string `json:"database"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body, err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want %q", body.Status, "ok")
	}
	if body.Database != "ok" {
		t.Errorf("database = %q, want %q", body.Database, "ok")
	}
	if body.Version == "" {
		t.Error("version is empty")
	}
}

// A pool pointed at a stopped database must report degraded rather than
// returning a 500 — the endpoint is what Kubernetes probes, so it has to stay
// answerable when its dependency is not.
func TestHealthReportsDegradedWhenDatabaseDown(t *testing.T) {
	pool := newTestDB(t)
	srv := api.New(api.Deps{Pool: pool, Log: discardLogger()})
	pool.Close()

	rec := httptest.NewRecorder()
	srv.Router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when degraded; body: %s", rec.Code, rec.Body)
	}
	var body struct {
		Status   string `json:"status"`
		Database string `json:"database"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "degraded" || body.Database != "unreachable" {
		t.Errorf("got status=%q database=%q, want degraded/unreachable", body.Status, body.Database)
	}
}
