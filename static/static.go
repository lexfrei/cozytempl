// Package static embeds the static assets for the web UI.
package static

import "embed"

// FS contains the embedded static files (css, dist).
//
//go:embed css dist
var FS embed.FS
