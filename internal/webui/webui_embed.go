//go:build embedweb

package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

// all: is required so that Vite's hashed assets and any dotfiles are included;
// the default embed pattern skips names beginning with "." or "_".
//
//go:embed all:dist
var dist embed.FS

func clientFS() http.FileSystem {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Only reachable if the embed directive and this path disagree, which
		// is a build-time mistake rather than a runtime condition.
		panic("webui: embedded client missing dist/: " + err.Error())
	}
	return http.FS(sub)
}
