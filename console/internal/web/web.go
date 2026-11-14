// Package web holds the console UI, built by docker/console.Dockerfile into dist/ before the Go build.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// UI is the static export. Everything the console shows comes from the binary, so it works offline.
func UI() fs.FS {
	ui, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return ui
}
