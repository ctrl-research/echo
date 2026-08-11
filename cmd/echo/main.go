// Command echo is the Echo music server.
//
// Subcommands:
//
//	echo serve      run the HTTP server (default)
//	echo migrate    apply pending migrations and exit
//	echo openapi    print the OpenAPI specification to stdout
//	echo version    print build identity
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jonathanng/echo/internal/api"
	"github.com/jonathanng/echo/internal/auth"
	"github.com/jonathanng/echo/internal/blobstore"
	"github.com/jonathanng/echo/internal/config"
	"github.com/jonathanng/echo/internal/db"
	"github.com/jonathanng/echo/internal/db/dbgen"
	"github.com/jonathanng/echo/internal/jobs"
	"github.com/jonathanng/echo/internal/library"
	"github.com/jonathanng/echo/internal/version"
	"github.com/jonathanng/echo/internal/webui"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	if err := run(cmd); err != nil {
		fmt.Fprintln(os.Stderr, "echo:", err)
		os.Exit(1)
	}
}

func run(cmd string) error {
	switch cmd {
	case "version":
		fmt.Println(version.String())
		return nil
	case "openapi":
		return runOpenAPI()
	case "serve", "migrate":
	default:
		return fmt.Errorf("unknown command %q (want serve, migrate, openapi, or version)", cmd)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	// Signal-aware from the outset so a hang during startup — a database that
	// never becomes reachable, say — still responds to Ctrl-C and to a
	// Kubernetes termination grace period.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("connected to database")

	if err := db.Migrate(ctx, pool, log); err != nil {
		return err
	}
	if cmd == "migrate" {
		return nil
	}

	queries := dbgen.New(pool)
	if cfg.LocalAuth {
		if err := auth.BootstrapLocalAdmin(ctx, queries, log,
			cfg.AdminEmail, cfg.AdminPassword); err != nil {
			return err
		}
	}

	// Discovery runs here, so a wrong issuer or unreachable identity provider
	// fails at startup rather than at somebody's first sign-in attempt.
	authSvc, err := auth.NewService(ctx, queries, log, auth.Options{
		BaseURL:            cfg.BaseURL,
		GoogleClientID:     cfg.GoogleClientID,
		GoogleClientSecret: cfg.GoogleClientSecret,
		OIDCIssuerURL:      cfg.OIDCIssuerURL,
		OIDCClientID:       cfg.OIDCClientID,
		OIDCClientSecret:   cfg.OIDCClientSecret,
		OIDCName:           cfg.OIDCName,
		LocalAuth:          cfg.LocalAuth,
		AllowedEmails:      cfg.AllowedEmails,
		SessionTTL:         cfg.SessionTTL,
	})
	if err != nil {
		return err
	}
	go authSvc.GCLoop(ctx)

	blobs, err := blobstore.NewLocal(cfg.CacheDir)
	if err != nil {
		return err
	}
	log.Info("cache ready", "dir", cfg.CacheDir)

	queue := jobs.New(pool, log)
	lib := library.NewService(pool, queue, blobs, log)
	lib.RegisterHandlers()

	// The writable root receives promoted YouTube downloads; the collection
	// roots are mounted read-only and never written to.
	var writableRoot string
	if n := len(cfg.LibraryRoots); n > 0 {
		writableRoot = cfg.LibraryRoots[n-1]
	}
	roots, err := lib.SyncRoots(ctx, cfg.LibraryRoots, writableRoot)
	if err != nil {
		return err
	}
	for _, r := range roots {
		log.Info("library root", "path", r.Path, "writable", r.Writable)
	}
	if len(roots) == 0 {
		log.Warn("no library roots configured; set ECHO_LIBRARY_ROOTS")
	}

	go queue.Run(ctx, cfg.ScanWorkers)

	// Watching must be a singleton: several watchers on one share would each
	// observe every event and enqueue duplicate work.
	if err := lib.StartWatcher(ctx); err != nil {
		log.Warn("filesystem watcher unavailable", "error", err)
	}

	if cfg.ScanOnStart {
		if n, err := lib.EnqueueScanAll(ctx); err != nil {
			log.Error("queue startup scan failed", "error", err)
		} else if n > 0 {
			log.Info("startup scan queued", "roots", n)
		}
	}

	if !authSvc.SecureCookies() {
		log.Warn("ECHO_BASE_URL is not https, so session cookies are not marked "+
			"Secure and will travel in the clear. Use this on a trusted LAN only.",
			"baseURL", cfg.BaseURL)
	}

	srv := api.New(api.Deps{
		Pool:    pool,
		Log:     log,
		Auth:    authSvc,
		Library: lib,
		WebFS:   webui.FS(),
	})

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Router,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: audio responses are long-lived streams and a write
		// deadline would sever playback mid-track.
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "version", version.String())
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// runOpenAPI prints the spec without touching the database, so that type
// generation works in CI and on a laptop with nothing running.
func runOpenAPI() error {
	srv := api.New(api.Deps{Log: slog.New(slog.DiscardHandler)})
	spec, err := srv.OpenAPI()
	if err != nil {
		return fmt.Errorf("render openapi: %w", err)
	}
	_, err = os.Stdout.Write(spec)
	return err
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}
