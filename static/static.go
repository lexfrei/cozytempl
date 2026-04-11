// Package static embeds the static assets for the web UI.
package static

import "embed"

// FS contains the embedded static files (css, dist, fonts).
//
//go:embed css dist fonts
var FS embed.FS
