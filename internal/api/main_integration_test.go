//go:build integration

package api_test

import (
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jonathanng/echo/internal/dbtest"
)

func TestMain(m *testing.M) { os.Exit(dbtest.Main(m)) }

func discardLogger() *slog.Logger          { return dbtest.DiscardLogger() }
func newTestDB(t *testing.T) *pgxpool.Pool { return dbtest.New(t) }
func newRawDB(t *testing.T) *pgxpool.Pool  { return dbtest.NewRaw(t) }
