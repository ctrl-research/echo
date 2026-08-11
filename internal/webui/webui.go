// Package webui exposes the built web client as an http.FileSystem.
//
// The client is only compiled into the binary under the `embedweb` build tag,
// which the Docker image sets after running the Vite build. A plain `go build`
// therefore works on a fresh checkout with no Node toolchain present, and
// local development runs the Vite dev server instead (see web/vite.config.ts,
// which proxies /api to this server).
package webui

import "net/http"

// FS returns the embedded client, or nil when the binary was built without it.
// api.Deps treats nil as "do not serve static files".
func FS() http.FileSystem { return clientFS() }
