package layout

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/lexfrei/cozytempl/internal/i18n"
)

// TestBaseEmitsSingleServerNowMarker pins the one-per-document
// contract for the LiveAge ticker's clock-skew anchor. The
// <body> element stamps data-server-now at render time with the
// server's wall clock in RFC 3339. The ticker reads exactly the
// first match; moving the attribute elsewhere (or emitting it
// per element) would re-introduce the SSE-row drift footgun
// this architecture was picked to avoid.
func TestBaseEmitsSingleServerNowMarker(t *testing.T) {
	t.Parallel()

	bundle, err := i18n.NewBundle()
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}

	// Base only reads the localizer's language tag (for
	// <html lang=>); a default-English localizer is enough
	// to exercise the body-level data-server-now stamp.
	loc := bundle.LocalizerFromContext(context.Background())

	got, err := renderToString(Base("Test", loc))
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if !strings.Contains(got, `<body data-server-now="`) {
		t.Errorf("<body> is missing data-server-now — LiveAge ticker loses its skew offset:\n%s", got)
	}

	// Exactly one occurrence is the invariant. A second
	// data-server-now in the tree would either be a mistake
	// by a future contributor or a sign that some SSE-stream
	// leak is rendering it per row; both break the ticker's
	// "read the first one" assumption.
	if count := strings.Count(got, `data-server-now=`); count != 1 {
		t.Errorf("expected exactly 1 data-server-now marker, got %d:\n%s", count, got)
	}

	// The timestamp must parse — a malformed RFC 3339 would
	// make the TS ticker fall back to the raw browser clock
	// instead of the server offset.
	markerIdx := strings.Index(got, `data-server-now="`)
	if markerIdx == -1 {
		t.Fatal("data-server-now not found; checked earlier but apparently missing here too")
	}

	start := markerIdx + len(`data-server-now="`)
	end := strings.Index(got[start:], `"`)
	if end <= 0 {
		t.Fatalf("data-server-now attribute is malformed:\n%s", got)
	}

	raw := got[start : start+end]
	if _, parseErr := time.Parse(time.RFC3339, raw); parseErr != nil {
		t.Errorf("data-server-now = %q, not a valid RFC 3339 timestamp: %v", raw, parseErr)
	}
}

// renderToString drives a templ component through Render and
// returns the resulting HTML as a string. Kept in this
// package instead of promoted to a shared helper because the
// other view packages already have their own copy — if a
// third caller shows up, lift the helper into a test-shared
// module.
func renderToString(c templ.Component) (string, error) {
	var b strings.Builder

	err := c.Render(context.Background(), &b)
	if err != nil {
		return "", fmt.Errorf("render templ: %w", err)
	}

	return b.String(), nil
}
