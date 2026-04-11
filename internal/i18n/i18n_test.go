package i18n

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/text/language"
)

// TestBundleLoadsAllLocales guards the shipping manifest: if a new
// SupportedLocales entry is added without a matching TOML file the
// test fails at load time, not at runtime for whichever user
// happened to pick that language.
func TestBundleLoadsAllLocales(t *testing.T) {
	t.Parallel()

	bundle, err := NewBundle()
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}

	// Every supported locale should resolve to a Localizer whose
	// T("nav.dashboard") returns a non-empty, non-bracketed value.
	for _, tag := range SupportedLocales {
		t.Run(tag.String(), func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			req.Header.Set("Accept-Language", tag.String())

			loc := bundle.resolve(req)

			got := loc.T("nav.dashboard")
			if got == "" {
				t.Errorf("nav.dashboard empty for %s", tag)
			}
			if got[0] == '[' {
				t.Errorf("nav.dashboard missing translation for %s: %q", tag, got)
			}
		})
	}
}

// TestLocaleResolutionPrecedence locks in the precedence chain:
// cookie beats Accept-Language, Accept-Language beats default.
// A future maintainer who "fixes" the order breaks this test
// and has to justify the change.
func TestLocaleResolutionPrecedence(t *testing.T) {
	t.Parallel()

	bundle, err := NewBundle()
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}

	tests := []struct {
		name      string
		cookie    string
		accept    string
		wantBase  language.Base
		wantFound bool
	}{
		{
			name:   "cookie wins over accept",
			cookie: "ru",
			accept: "zh",
			wantBase: func() language.Base {
				b, _ := language.Russian.Base()
				return b
			}(),
			wantFound: true,
		},
		{
			name:   "accept used when no cookie",
			cookie: "",
			accept: "kk",
			wantBase: func() language.Base {
				b, _ := language.Kazakh.Base()
				return b
			}(),
			wantFound: true,
		},
		{
			name:   "unknown cookie falls through to accept",
			cookie: "xx",
			accept: "zh",
			wantBase: func() language.Base {
				b, _ := language.Chinese.Base()
				return b
			}(),
			wantFound: true,
		},
		{
			name:   "nothing set yields english default",
			cookie: "",
			accept: "",
			wantBase: func() language.Base {
				b, _ := language.English.Base()
				return b
			}(),
			wantFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			if tt.cookie != "" {
				req.AddCookie(&http.Cookie{Name: cookieName, Value: tt.cookie})
			}
			if tt.accept != "" {
				req.Header.Set("Accept-Language", tt.accept)
			}

			loc := bundle.resolve(req)

			gotBase, _ := loc.Tag().Base()
			if gotBase != tt.wantBase {
				t.Errorf("resolved base = %s, want %s (tag=%s)", gotBase, tt.wantBase, loc.Tag())
			}
		})
	}
}

// TestMissingMessageIDReturnsBracketedID guards the
// "never silent on missing keys" invariant. A typo in a templ
// file should show up as "[nav.oops]" on the page instead of an
// empty string that hides the mistake.
func TestMissingMessageIDReturnsBracketedID(t *testing.T) {
	t.Parallel()

	bundle, err := NewBundle()
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)

	loc := bundle.resolve(req)

	got := loc.T("does.not.exist")
	if got != "[does.not.exist]" {
		t.Errorf("missing id got %q, want %q", got, "[does.not.exist]")
	}
}

// TestTemplateDataSubstitutionWorks covers the {{.Name}}-style
// placeholders in message strings. The Signed-in-as header label
// uses it, as do every error page message — breaking it would
// replace usernames with literal "{{.Name}}" in the UI.
func TestTemplateDataSubstitutionWorks(t *testing.T) {
	t.Parallel()

	bundle, err := NewBundle()
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)

	loc := bundle.resolve(req)

	got := loc.T("header.signedInAs", map[string]any{"Name": "alice"})
	if got == "" || got == "[header.signedInAs]" {
		t.Fatalf("substitution failed: %q", got)
	}
	// Must contain the substituted name and no raw placeholder.
	if !containsString(got, "alice") {
		t.Errorf("result missing substituted name: %q", got)
	}
	if containsString(got, "{{.Name}}") {
		t.Errorf("result still contains raw placeholder: %q", got)
	}
}

// containsString is a tiny helper used only by the test above to
// avoid dragging strings.Contains into the test imports for a
// single call — keeps the imports block minimal.
func containsString(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}
