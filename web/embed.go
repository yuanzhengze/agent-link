// Package web owns the go:embed of the cowork dashboard's static assets.
// It lives at the repo root (not under pkg/api) because go:embed paths are
// relative to the embedding file's directory and cannot reference parent
// directories — pkg/api/server.go imports this package to mount the GUI at
// GET /.
package web

import "embed"

//go:embed index.html app.js api.js auth.js teams.js dashboard.js style.css
var FS embed.FS
