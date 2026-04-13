package page

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view"
)

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

	if !strings.Contains(got, `value="Etcd" selected`) {
		t.Errorf(`kind option "Etcd" should be selected; output:\n%s`, got)
	}

	if !strings.Contains(got, `/fragments/schema-fields?kind=Etcd`) {
		t.Errorf("schema-fields auto-load URL missing; output:\n%s", got)
	}
	if !strings.Contains(got, `hx-trigger="load"`) {
		t.Errorf(`schema-fields should hx-trigger="load"; output:\n%s`, got)
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
}
