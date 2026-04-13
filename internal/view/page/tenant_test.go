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
