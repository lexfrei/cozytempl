package page

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lexfrei/cozytempl/internal/actions"
	"github.com/lexfrei/cozytempl/internal/i18n"
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

// TestResourceActionsBarSkipsDivWhenEmpty pins the contract the
// cycle-6 review flagged: when no actions are registered for the
// Kind, the page must NOT emit a wrapping div. The previous
// docstring claimed a "zero-sized div" which the code never
// produced — this test locks the code side of that mismatch.
func TestResourceActionsBarSkipsDivWhenEmpty(t *testing.T) {
	t.Parallel()

	data := view.AppDetailData{
		Tenant:         "tenant-root",
		App:            k8s.Application{Name: "redis-1", Kind: "Redis"},
		Tab:            "overview",
		AllowedActions: nil, // no actions registered for this Kind
	}

	var buf bytes.Buffer
	if err := AppDetail(data).Render(context.Background(), &buf); err != nil {
		t.Fatalf("rendering AppDetail: %v", err)
	}

	if strings.Contains(buf.String(), "page-header-actions") {
		t.Errorf("empty AllowedActions should not emit page-header-actions div; body = %s", buf.String())
	}
}

// TestResourceActionsBarConfirmAttrEscapes renders the destructive
// action buttons under every supported locale and asserts that the
// hx-confirm attribute value carries no literal unescaped double
// quote. If a future translator writes a quoted label in their copy
// (e.g. `Выполнить «{{.Label}}»?` → `Выполнить "Stop"?`), templ's
// attribute escape must convert that `"` into `&#34;`, otherwise the
// browser would truncate the attribute value at the first `"` and
// silently break the confirm prompt. Today all four locales are
// safe, but regressions are cheap to catch.
func TestResourceActionsBarConfirmAttrEscapes(t *testing.T) {
	t.Parallel()

	bundle, err := i18n.NewBundle()
	if err != nil {
		t.Fatalf("loading i18n bundle: %v", err)
	}

	// A label that embeds a double quote — forces templ's attribute
	// escape to do real work rather than silently succeed because
	// the translated string happens to be quote-free.
	quotedAction := actions.Action{
		ID:          "stop-evil",
		LabelKey:    `"STOP"`, // non-existent key → partial.Tc returns it verbatim, quotes intact
		Destructive: true,
	}

	for _, locale := range i18n.SupportedLocales {
		t.Run(locale.String(), func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			req.Header.Set("Accept-Language", locale.String())

			var buf bytes.Buffer

			// Run through Middleware to attach a Localizer to ctx.
			bundle.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				if err := resourceActionButton("t", "vm-42", quotedAction).Render(r.Context(), &buf); err != nil {
					t.Fatalf("rendering button: %v", err)
				}
			})).ServeHTTP(httptest.NewRecorder(), req)

			html := buf.String()

			confirmAttr := extractAttr(html, "hx-confirm")
			if confirmAttr == "" {
				t.Fatalf("hx-confirm not rendered under %s: %s", locale, html)
			}

			if strings.Contains(confirmAttr, `"`) {
				t.Errorf("hx-confirm under %s contains literal unescaped double quote: %q (full html: %s)",
					locale, confirmAttr, html)
			}
		})
	}
}

// extractAttr pulls the value of attr from html, returning the
// string between the opening `"` and closing `"` of the first match.
// Returns "" when the attribute is absent. Deliberately naive — the
// caller passes a single rendered button, not an arbitrary document.
func extractAttr(html, attr string) string {
	needle := attr + `="`

	start := strings.Index(html, needle)
	if start == -1 {
		return ""
	}

	start += len(needle)

	end := strings.Index(html[start:], `"`)
	if end == -1 {
		return ""
	}

	return html[start : start+end]
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
