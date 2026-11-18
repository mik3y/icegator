// Package web holds the built dashboard app so that it ships inside the
// icegator binary.
package web

import (
	"embed"
	"io/fs"
)

// The dist directory is filled by `bun run build`. Only a placeholder is
// checked in, so a binary built without that step has no dashboard app.
//
//go:embed all:dist
var dist embed.FS

// App returns the built dashboard, or nil if it was not built into this
// binary.
func App() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
