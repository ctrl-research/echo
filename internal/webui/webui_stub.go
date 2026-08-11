//go:build !embedweb

package webui

import "net/http"

// Built without the client. Only the API is served.
func clientFS() http.FileSystem { return nil }
