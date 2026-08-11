//go:build integration

// Package dbtest provides a PostgreSQL container for integration tests.
//
// One container serves a whole test binary. Each test gets its own database
// cloned from a pre-migrated template, which is dramatically faster than a
// container per test and still gives complete isolation — tests can create
// users and sessions without coordinating.
//
// Behind the `integration` build tag so that a plain `go build ./...` neither
// compiles it nor pulls testcontainers into the dependency graph.
package dbtest

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/jonathanng/echo/internal/db"
)

const templateDB = "echo_template"

var (
	// adminURL points at the container's default database and is used only to
	// CREATE and DROP the per-test databases.
	adminURL string
	dbCount  atomic.Int64
)

// DiscardLogger is a logger that writes nothing, for tests that do not assert
// on log output.
func DiscardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// Main starts the container, builds the template database, and runs m.
// Call it from TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(dbtest.Main(m)) }
func Main(m *testing.M) int {
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("echo"),
		tcpostgres.WithUsername("echo"),
		tcpostgres.WithPassword("echo"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(90*time.Second),
				// The log strategy only proves Postgres is up inside the
				// container. On Docker hosts that run in a VM — colima, Docker
				// Desktop, Rancher — the published port becomes reachable from
				// the host a moment later, so a host-side dial is what actually
				// makes the connection string usable.
				wait.ForListeningPort("5432/tcp").
					WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dbtest: start postgres: %v\n", err)
		return 1
	}
	defer func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			fmt.Fprintf(os.Stderr, "dbtest: terminate postgres: %v\n", err)
		}
	}()

	adminURL, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "dbtest: connection string: %v\n", err)
		return 1
	}
	if err := buildTemplate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest: build template: %v\n", err)
		return 1
	}
	return m.Run()
}

// buildTemplate creates a database with the full schema applied, to be cloned
// per test. The pool is closed afterwards because CREATE DATABASE ... TEMPLATE
// fails while any session is connected to the template.
func buildTemplate(ctx context.Context) error {
	admin, err := db.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer admin.Close()

	if _, err := admin.Exec(ctx, `CREATE DATABASE `+templateDB); err != nil {
		return fmt.Errorf("create template: %w", err)
	}

	tmpl, err := db.Connect(ctx, URLFor(templateDB))
	if err != nil {
		return err
	}
	defer tmpl.Close()
	if err := db.Migrate(ctx, tmpl, DiscardLogger()); err != nil {
		return fmt.Errorf("migrate template: %w", err)
	}
	return nil
}

// URLFor returns a connection string for a named database on the container.
func URLFor(name string) string {
	u, err := url.Parse(adminURL)
	if err != nil {
		panic("dbtest: parse admin url: " + err.Error())
	}
	u.Path = "/" + name
	return u.String()
}

// New returns a pool for a fresh database with the schema already applied.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return clone(t, templateDB)
}

// NewRaw returns a pool for an empty database with no migrations applied, for
// tests that exercise migration behaviour itself.
func NewRaw(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return clone(t, "")
}

func clone(t *testing.T, template string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	name := fmt.Sprintf("echo_test_%d", dbCount.Add(1))

	admin, err := db.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("dbtest: connect admin: %v", err)
	}
	defer admin.Close()

	stmt := `CREATE DATABASE ` + name
	if template != "" {
		stmt += ` TEMPLATE ` + template
	}
	if _, err := admin.Exec(ctx, stmt); err != nil {
		t.Fatalf("dbtest: create database %s: %v", name, err)
	}

	pool, err := db.Connect(ctx, URLFor(name))
	if err != nil {
		t.Fatalf("dbtest: connect %s: %v", name, err)
	}

	t.Cleanup(func() {
		pool.Close()
		cleanup, err := db.Connect(context.Background(), adminURL)
		if err != nil {
			t.Logf("dbtest: drop %s: connect: %v", name, err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(context.Background(),
			`DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Logf("dbtest: drop %s: %v", name, err)
		}
	})

	return pool
}
