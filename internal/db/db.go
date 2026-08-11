// Package db owns the Postgres connection pool and schema migrations.
package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Connect opens a pool and verifies the database is reachable.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	// The library scanner is the heaviest writer and runs with a bounded
	// worker pool; these defaults leave room for it alongside request traffic
	// without letting a runaway component exhaust the server's connections.
	if cfg.MaxConns == 0 {
		cfg.MaxConns = 16
	}
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Migrate applies all pending migrations.
//
// A Postgres session-level advisory lock serialises this across processes, so
// a rolling deployment or a compose stack that starts several components at
// once cannot run goose concurrently against the same database.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("open migrations fs: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	store, err := database.NewStore(database.DialectPostgres, goose.DefaultTablename)
	if err != nil {
		return fmt.Errorf("create migration store: %w", err)
	}

	locker, err := lock.NewPostgresSessionLocker(
		lock.WithLockTimeout(10, 60),   // probe every 10s for up to 10 minutes
		lock.WithUnlockTimeout(10, 60), //
	)
	if err != nil {
		return fmt.Errorf("create session locker: %w", err)
	}

	provider, err := goose.NewProvider("", sqlDB, sub,
		goose.WithStore(store),
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	for _, r := range results {
		log.Info("migration applied", "version", r.Source.Version, "name", r.Source.Path, "duration", r.Duration)
	}
	if len(results) == 0 {
		log.Info("schema up to date")
	}
	return nil
}
