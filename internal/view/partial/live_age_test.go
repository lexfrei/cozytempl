package partial

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
)

// renderTemplToString drives a templ component through its
// Render method so tests can assert on the HTML byte-for-byte.
// Hides the goroutine + pipe dance behind a plain string
// return value; errors bubble so tests can t.Fatalf.
func renderTemplToString(c templ.Component) (string, error) {
	var b strings.Builder

	err := c.Render(context.Background(), &b)
	if err != nil {
		return "", fmt.Errorf("render templ component: %w", err)
	}

	return b.String(), nil
}

// containsSubstring is a one-liner used by the test block to
// keep the assertion lines readable — multiple strings.Contains
// calls with long HTML arguments drift past the 140-col lint
// cap; a helper trims them to one identifier.
func containsSubstring(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

// TestHumanizeAgeBoundaries pins the kubectl-parity thresholds
// the ticker depends on: the unit flips at 60s, 60m, 24h,
// 365d. A regression that e.g. kept seconds for 90s would
// make the age column read as "90s" instead of "1m" and drift
// from `kubectl get` on the same resource.
func TestHumanizeAgeBoundaries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "0s"},
		{"sub-second", 500 * time.Millisecond, "0s"},
		{"exactly-one-second", time.Second, "1s"},
		{"42s", 42 * time.Second, "42s"},
		{"59s", 59 * time.Second, "59s"},
		{"minute-boundary", 60 * time.Second, "1m"},
		{"3m", 3 * time.Minute, "3m"},
		{"59m", 59 * time.Minute, "59m"},
		{"hour-boundary", time.Hour, "1h"},
		{"23h", 23 * time.Hour, "23h"},
		{"day-boundary", 24 * time.Hour, "1d"},
		{"12d", 12 * 24 * time.Hour, "12d"},
		{"364d", 364 * 24 * time.Hour, "364d"},
		{"year-boundary", 365 * 24 * time.Hour, "1y"},
		{"3y-ish", 3 * 365 * 24 * time.Hour, "3y"},
		{"negative-duration", -5 * time.Second, "0s"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := HumanizeAge(tc.d)
			if got != tc.want {
				t.Errorf("HumanizeAge(%s) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}

// TestLiveAgeRendersDataAttribute confirms the component
// emits the RFC 3339 timestamp a client-side ticker can read.
// Without the data-age-start attribute the TypeScript module
// has nothing to re-render against and the value freezes at
// first paint — the exact regression this test catches.
func TestLiveAgeRendersDataAttribute(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)

	got, err := renderTemplToString(LiveAge(created))
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if !containsSubstring(got, `data-age-start="2026-01-15T10:30:00Z"`) {
		t.Errorf("output missing data-age-start RFC3339 attribute:\n%s", got)
	}

	if !containsSubstring(got, `datetime="2026-01-15T10:30:00Z"`) {
		t.Errorf("output missing <time datetime> attribute for a11y:\n%s", got)
	}

	if !containsSubstring(got, `class="live-age"`) {
		t.Errorf("output missing live-age class — ticker selector would miss this element:\n%s", got)
	}

	// The clock-skew marker (data-server-now) is emitted
	// once per document on <body>, NOT per live-age element.
	// A regression that moved the marker back onto the
	// <time> would inflate the wire on pages with hundreds
	// of age columns and create a moving-target footgun for
	// SSE-injected rows. Guard against it here.
	if containsSubstring(got, `data-server-now`) {
		t.Errorf("LiveAge must not emit data-server-now; the base layout owns the one-per-document marker:\n%s", got)
	}
}

// TestLiveAgeDoesNotLeakServerTimezoneIntoTitle pins the
// timezone regression: the absolute tooltip is NOT rendered
// server-side because the server does not know the user's
// timezone (container is usually UTC, sometimes whatever the
// Go binary was built with). A user in Berlin seeing a
// US-format UTC timestamp on hover was the specific UX
// regression the previous formatTime() path carried forward
// by accident. The TypeScript ticker now populates title= on
// init using toLocaleString so the tooltip respects the
// user's browser locale + system timezone. Guard against a
// future revision accidentally re-adding title= on the
// server side.
func TestLiveAgeDoesNotLeakServerTimezoneIntoTitle(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)

	got, err := renderTemplToString(LiveAge(created))
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if containsSubstring(got, `title="`) {
		t.Errorf("server emitted title=; timezone belongs to client locale, not server:\n%s", got)
	}
}

// TestLiveAgeZeroTimestampRendersNothing pins the empty-cell
// behaviour: a never-set timestamp is common for resources
// the apiserver has not yet populated (just-created, status
// subresource not yet reconciled), and rendering "0s" there
// is worse than rendering nothing — the "age" reads as "the
// resource is 0 seconds old" when it really means "we have no
// timestamp yet". An empty cell is the honest answer.
func TestLiveAgeZeroTimestampRendersNothing(t *testing.T) {
	t.Parallel()

	got, err := renderTemplToString(LiveAge(time.Time{}))
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if got != "" {
		t.Errorf("zero timestamp should render nothing, got %q", got)
	}
}
