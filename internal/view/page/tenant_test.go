package page

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view"
)

// etcdOptionTag matches the entire <option …> tag for value="Etcd".
// The body of the tag is then inspected separately for any form of the
// selected attribute — `selected`, `selected=""`, `selected="selected"`
// — so the test passes regardless of how templ chooses to emit a
// boolean attribute. Anchoring on the open tag keeps the assertion off
// adjacent <option> elements with different values.
var etcdOptionTag = regexp.MustCompile(`<option[^>]*value="Etcd"[^>]*>`)

// findTagWithAttribute returns the first opening tag of `name` whose
// attribute substring contains marker, or "" if none matches. Used by
// regression assertions that pin specific attributes on a specific
// element rather than matching anywhere in the rendered output.
func findTagWithAttribute(html, name, marker string) string {
	pattern := regexp.MustCompile(`<` + name + `[^>]*` + regexp.QuoteMeta(marker) + `[^>]*>`)
	return pattern.FindString(html)
}

// optionIsSelected reports whether the <option> tag matched by
// etcdOptionTag carries a selected attribute in any spelling.
func optionIsSelected(tag string) bool {
	switch {
	case strings.Contains(tag, ` selected `),
		strings.Contains(tag, ` selected>`),
		strings.Contains(tag, ` selected/`),
		strings.Contains(tag, `selected=""`),
		strings.Contains(tag, `selected="selected"`):
		return true
	}

	return false
}

func sampleTenantPageData(createKind string) view.TenantPageData {
	return view.TenantPageData{
		Tenant: k8s.Tenant{
			Name:            "demo",
			Namespace:       "tenant-demo",
			ParentNamespace: "tenant-root",
			DisplayName:     "demo",
		},
		Schemas: []k8s.AppSchema{
			{Kind: "Etcd", DisplayName: "Etcd"},
			{Kind: "Redis", DisplayName: "Redis"},
		},
		CreateKind: createKind,
	}
}

// TestTenantAutoOpensCreateModalWithCreateKind checks the user flow from
// marketplace: when CreateKind is set, the create-app modal must render
// already open, with the matching <option> marked selected, and the
// schema-fields container primed to htmx-fetch the form on load. Any of
// the three missing means the user still has to click around after
// landing on the page.
func TestTenantAutoOpensCreateModalWithCreateKind(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := Tenant(sampleTenantPageData("Etcd")).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}

	got := buf.String()

	modalIdx := strings.Index(got, `id="create-app-modal"`)
	if modalIdx < 0 {
		t.Fatalf("create-app-modal not found; got:\n%s", got)
	}
	modalTag := got[modalIdx:]
	if endIdx := strings.Index(modalTag, ">"); endIdx > 0 {
		modalTag = modalTag[:endIdx]
	}
	if !strings.Contains(modalTag, "display: flex") {
		t.Errorf("create-app-modal should be open (display: flex); got opening tag: %s", modalTag)
	}

	tag := etcdOptionTag.FindString(got)
	if tag == "" {
		t.Fatalf(`<option value="Etcd"> not found; output:\n%s`, got)
	}
	if !optionIsSelected(tag) {
		t.Errorf(`<option value="Etcd"> should be selected; tag was: %s`, tag)
	}

	if !strings.Contains(got, `/fragments/schema-fields?kind=Etcd`) {
		t.Errorf("schema-fields auto-load URL missing; output:\n%s", got)
	}
	if !strings.Contains(got, `hx-trigger="load"`) {
		t.Errorf(`schema-fields should hx-trigger="load"; output:\n%s`, got)
	}

	// Accessibility: auto-opened modal should move keyboard focus into
	// the form, not leave it on <body>. The name <input> is the first
	// editable field so autofocus goes there.
	if !strings.Contains(got, `autofocus`) {
		t.Errorf("auto-opened modal should autofocus the name input; output:\n%s", got)
	}

	// UX: the schema-fields fetch is async. Render a placeholder so the
	// user sees the modal is doing something instead of a blank body.
	if !strings.Contains(got, `[page.tenant.loadingFields]`) {
		t.Errorf("schema-fields placeholder (i18n key) missing; output:\n%s", got)
	}

	// REGRESSION: the schema-fields auto-loader sits inside a <form
	// hx-target="#main-content"> (the create-app submit target). Without
	// an explicit hx-target on the auto-loader div, htmx attribute
	// inheritance applies #main-content to the auto-fetch and the
	// schema-fields fragment replaces the entire tenant page on first
	// paint — user lands on /tenants/{ns}?createKind=… and sees only
	// the raw schema rows with no chrome. The fix pins hx-target="this"
	// so the swap stays inside the modal. Matched by hx-trigger="load"
	// now that the div is anonymous: its id used to be "schema-fields",
	// but that bare id collided with the edit modal's equivalent
	// container after Apply-to-Form, so ids are now scoped per-bodyID
	// one level up at AppFormTabs (#create-app-form-schema-fields).
	autoLoaderTag := findTagWithAttribute(got, "div", `hx-trigger="load"`)
	if autoLoaderTag == "" {
		t.Fatalf(`schema-fields auto-loader div not found; output:\n%s`, got)
	}

	if !strings.Contains(autoLoaderTag, `hx-target="this"`) {
		t.Errorf(`schema-fields auto-loader must set hx-target="this" to opt out of inherited #main-content target; tag was:\n%s`, autoLoaderTag)
	}

	// REGRESSION: the auto-loader must NOT carry id="schema-fields".
	// Both the create and edit modals can coexist in the DOM once the
	// user has opened one and then the other; a bare id would land
	// htmx selectors on the wrong modal (first document-order match).
	// The scoped outer container (id="create-app-form-schema-fields",
	// rendered by AppFormTabs) is the one-and-only container any
	// htmx attribute should target.
	if strings.Contains(autoLoaderTag, `id="schema-fields"`) {
		t.Errorf(
			"auto-loader must not declare bare id=\"schema-fields\""+
				" (would collide with edit modal after Apply-to-Form); tag was:\n%s",
			autoLoaderTag)
	}

	if !strings.Contains(got, `id="create-app-form-schema-fields"`) {
		t.Errorf(
			"create modal must carry scoped id=\"create-app-form-schema-fields\""+
				" on the form pane container; output:\n%s", got)
	}

	// REGRESSION: the kind-select must target the scoped container,
	// not the bare #schema-fields — a bare target picks up the first
	// matching element in document order, which can be the edit
	// modal when both modals are in the DOM at once.
	if !strings.Contains(got, `hx-target="#create-app-form-schema-fields"`) {
		t.Errorf(`kind-select must hx-target="#create-app-form-schema-fields"; output:\n%s`, got)
	}
}

// TestCreateAppModalStyle switches between the two inline styles based
// on whether a kind was preselected. The helper only ever returns
// SafeCSS literals, but locking the mapping keeps a future edit from
// silently flipping the default state.
func TestCreateAppModalStyle(t *testing.T) {
	t.Parallel()

	if got := string(createAppModalStyle("Etcd")); got != "display: flex;" {
		t.Errorf("createAppModalStyle(non-empty) = %q, want display: flex;", got)
	}
	if got := string(createAppModalStyle("")); got != "display: none;" {
		t.Errorf("createAppModalStyle(empty) = %q, want display: none;", got)
	}
}

// TestSchemaFieldsAutoLoadURL covers the htmx fetch target builder.
// The handler already strips unknown kinds via selectKnownKind, but the
// helper still escapes so any leak emits a safe query string.
func TestSchemaFieldsAutoLoadURL(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"Etcd":        "/fragments/schema-fields?kind=Etcd",
		"Etcd&evil=1": "/fragments/schema-fields?kind=Etcd%26evil%3D1",
		"Et cd":       "/fragments/schema-fields?kind=Et+cd",
	}

	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			got := schemaFieldsAutoLoadURL(input)
			if got != want {
				t.Errorf("schemaFieldsAutoLoadURL(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

// TestTenantModalHiddenByDefault locks in the untouched behaviour when a
// user navigates directly to /tenants/{ns} without the createKind query
// param — no modal pops open and no automatic schema fetch fires.
func TestTenantModalHiddenByDefault(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := Tenant(sampleTenantPageData("")).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}

	got := buf.String()

	modalIdx := strings.Index(got, `id="create-app-modal"`)
	if modalIdx < 0 {
		t.Fatalf("create-app-modal not found; got:\n%s", got)
	}
	modalTag := got[modalIdx:]
	if endIdx := strings.Index(modalTag, ">"); endIdx > 0 {
		modalTag = modalTag[:endIdx]
	}
	if !strings.Contains(modalTag, "display: none") {
		t.Errorf("create-app-modal should be hidden (display: none); got opening tag: %s", modalTag)
	}

	if strings.Contains(got, `hx-trigger="load"`) {
		t.Errorf(`schema-fields should not auto-load without createKind; output:\n%s`, got)
	}

	// Without CreateKind the user opens the modal manually, so the
	// click target already has focus — we must not steal it with a
	// silent autofocus on the input behind a hidden modal.
	if strings.Contains(got, `autofocus`) {
		t.Errorf("default render should not set autofocus; output:\n%s", got)
	}
}
