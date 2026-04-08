// Package static embeds the static assets for the web UI.
package static

import "embed"

// FS contains the embedded static files (css, js).
//
//go:embed css js
var FS embed.FS
