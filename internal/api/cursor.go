package api

import (
	"encoding/base64"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/jonathanng/echo/internal/library"
)

// Cursors are opaque to clients on purpose. Encoding the sort key and id means
// a page boundary survives rows being inserted or removed mid-scroll — which
// happens constantly while a scan runs — where an offset would silently skip
// or repeat items.
//
// They are not encrypted, only encoded: nothing here is secret, and the
// alternative is a client tempted to construct one by hand.
type cursor struct {
	Sort string
	ID   uuid.UUID
}

var errBadCursor = errors.New("malformed cursor")

func encodeCursor(sort string, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(sort + "\x00" + id.String()))
}

func decodeCursor(s string) (cursor, error) {
	if s == "" {
		return cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, errBadCursor
	}
	sortKey, idPart, found := strings.Cut(string(raw), "\x00")
	if !found {
		return cursor{}, errBadCursor
	}
	id, err := uuid.Parse(idPart)
	if err != nil {
		return cursor{}, errBadCursor
	}
	return cursor{Sort: sortKey, ID: id}, nil
}

// page trims an over-fetched result set to the requested size and returns the
// cursor for the next page.
//
// Listings ask the database for limit+1 rows: if the extra row comes back there
// is more to fetch, which is how "is there a next page" is answered without a
// second COUNT over the whole filtered set.
func page[T any](rows []T, limit int, key func(T) (string, uuid.UUID)) ([]T, string) {
	if len(rows) <= limit {
		return rows, ""
	}
	rows = rows[:limit]
	sortKey, id := key(rows[len(rows)-1])
	return rows, encodeCursor(sortKey, id)
}

// normalizeForCursor mirrors the scanner's name normalisation, which is what
// artists and albums are sorted and matched on. Re-exported through the api
// package so handlers do not each reach into the library package for it.
func normalizeForCursor(s string) string { return library.Normalize(s) }
