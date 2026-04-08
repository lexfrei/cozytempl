// Package static embeds the static assets for the web UI.
package static

import "embed"

// FS contains the embedded static files (css, js, dist).
//
//go:embed css js dist
var FS embed.FS
