package page

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/lexfrei/cozytempl/internal/actions"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view"
)

// TestResourceActionsBarDestructiveConfirms pins two properties of
// the action bar that the review flagged:
//
//   - Destructive actions (Stop, Restart) must carry hx-confirm so
//     an accidental click cannot silently cause an outage.
//   - Destructive actions render with the btn-danger variant so the
//     colour matches the semantics.
//
// A regression on either of these drops back to the "three
// identical grey buttons" layout that made the first reviewer push
// back. Keep this test; it's cheap coverage for UX contract.
func TestResourceActionsBarDestructiveConfirms(t *testing.T) {
	t.Parallel()

	data := view.AppDetailData{
		Tenant: "tenant-root",
		App: k8s.Application{
			Name: "vm-42",
			Kind: "VMInstance",
		},
		Tab:            "overview",
		AllowedActions: actions.For("VMInstance"),
	}

	var buf bytes.Buffer
	if err := AppDetail(data).Render(context.Background(), &buf); err != nil {
		t.Fatalf("rendering AppDetail: %v", err)
	}

	html := buf.String()

	// Start is safe; render as primary, no hx-confirm.
	startTag := firstTagContaining(html, "button", "page.appDetail.action.vmStart")
	if startTag == "" {
		t.Fatal("Start button not rendered; AllowedActions wiring regressed")
	}

	if strings.Contains(startTag, "hx-confirm=") {
		t.Errorf("Start button MUST NOT carry hx-confirm (safe action); tag was %s", startTag)
	}

	if !strings.Contains(startTag, "btn-primary") {
		t.Errorf("Start button should render as btn-primary; tag was %s", startTag)
	}

	// Stop + Restart are destructive; require confirm + danger styling.
	for _, labelKey := range []string{
		"page.appDetail.action.vmStop",
		"page.appDetail.action.vmRestart",
	} {
		tag := firstTagContaining(html, "button", labelKey)
		if tag == "" {
			t.Errorf("destructive button for %s not rendered", labelKey)

			continue
		}

		if !strings.Contains(tag, "hx-confirm=") {
			t.Errorf("%s MUST carry hx-confirm (destructive); tag was %s", labelKey, tag)
		}

		if !strings.Contains(tag, "btn-danger") {
			t.Errorf("%s should render as btn-danger; tag was %s", labelKey, tag)
		}
	}
}

// firstTagContaining returns the full opening tag of the first
// element of `name` whose text content or attribute values include
// marker, or "" if nothing matches. Naive — scans left-to-right —
// but enough to pin per-element attributes in unit tests without
// dragging in a full HTML parser.
func firstTagContaining(html, name, marker string) string {
	searchFrom := 0
	open := "<" + name

	for {
		start := strings.Index(html[searchFrom:], open)
		if start == -1 {
			return ""
		}

		start += searchFrom

		end := strings.Index(html[start:], ">")
		if end == -1 {
			return ""
		}

		end += start + 1

		// Include the closing </button> text so a marker in the
		// button's visible label (e.g. an i18n key) still matches.
		closeIdx := strings.Index(html[end:], "</"+name+">")
		if closeIdx == -1 {
			return ""
		}

		fullElement := html[start : end+closeIdx]

		if strings.Contains(fullElement, marker) {
			return html[start:end]
		}

		searchFrom = end
	}
}
