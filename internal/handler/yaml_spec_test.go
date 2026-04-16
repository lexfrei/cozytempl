package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseSpecYAMLRoundtripsMapTypes locks the contract
// buildSpecFromRequest relies on: sigs.k8s.io/yaml must decode
// YAML into Go's native JSON-shaped map[string]any (integers as
// float64 / int, booleans as bool, strings as strings). A
// regression to gopkg.in/yaml.v3 would coerce integers to int
// but nested maps to map[interface{}]interface{}, which the
// downstream JSON marshal in appSvc.Create would break on.
func TestParseSpecYAMLRoundtripsMapTypes(t *testing.T) {
	t.Parallel()

	raw := `replicas: 3
enabled: true
storage:
  class: fast
  size: 10Gi
`

	spec, err := parseSpecYAML(raw)
	if err != nil {
		t.Fatalf("parseSpecYAML: %v", err)
	}

	if spec["replicas"] != float64(3) && spec["replicas"] != int64(3) {
		t.Errorf("replicas = %v (%T), want numeric 3", spec["replicas"], spec["replicas"])
	}

	if spec["enabled"] != true {
		t.Errorf("enabled = %v, want true", spec["enabled"])
	}

	storage, ok := spec["storage"].(map[string]any)
	if !ok {
		t.Fatalf("storage wrong type: %T; must be map[string]any", spec["storage"])
	}

	if storage["class"] != "fast" {
		t.Errorf("storage.class = %v, want fast", storage["class"])
	}
}

// TestParseSpecYAMLRejectsGarbage confirms the error path so
// the caller can surface "invalid YAML" to the user instead of
// happily POSTing a half-parsed spec. Drives a token the
// parser cannot make sense of in any context.
func TestParseSpecYAMLRejectsGarbage(t *testing.T) {
	t.Parallel()

	_, err := parseSpecYAML("this is: [not: valid: yaml")
	if err == nil {
		t.Fatal("expected parse error on malformed YAML")
	}

	if !errors.Is(err, ErrInvalidYAMLSpec) {
		t.Errorf("err = %v, want wraps ErrInvalidYAMLSpec", err)
	}
}

// TestBuildSpecFromRequestPrefersYAMLWhenTabModeYAML pins the
// tab-driven source selection. When _tabmode=yaml (the user
// explicitly chose the YAML pane) the server picks YAML even
// if form fields are present — and it picks Form when
// _tabmode=form, even if the YAML textarea is still populated
// from an earlier "Load from Form" / "Apply to Form". Without
// _tabmode driving the decision a user who applied YAML,
// switched back to Form, tweaked values, and pressed Save
// would see their form edits silently discarded because the
// (stale) YAML still won.
func TestBuildSpecFromRequestPrefersYAMLWhenTabModeYAML(t *testing.T) {
	t.Parallel()

	form := strings.NewReader("_tabmode=yaml&name=pg&kind=Postgres&replicas=99&spec_yaml=replicas%3A%203")

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/tenants/ns/apps", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if parseErr := req.ParseForm(); parseErr != nil {
		t.Fatalf("ParseForm: %v", parseErr)
	}

	pgh := &PageHandler{}

	spec, err := pgh.buildSpecFromRequest(req, nil, "Postgres")
	if err != nil {
		t.Fatalf("buildSpecFromRequest: %v", err)
	}

	assertReplicas(t, spec, 3, "YAML should win when _tabmode=yaml")
}

// assertReplicas extracts the replicas key tolerantly of
// whether sigs.k8s.io/yaml decoded it as int64 or float64.
func assertReplicas(t *testing.T, spec map[string]any, want float64, msg string) {
	t.Helper()

	switch v := spec["replicas"].(type) {
	case float64:
		if v != want {
			t.Errorf("%s: replicas = %v, want %v", msg, v, want)
		}
	case int64:
		if float64(v) != want {
			t.Errorf("%s: replicas = %v, want %v", msg, v, want)
		}
	default:
		t.Errorf("%s: replicas wrong type: %T", msg, spec["replicas"])
	}
}

// TestBuildSpecFromRequestSurfacesYAMLError pins that a bad
// spec_yaml does not silently fall through to the form branch.
// A user on the YAML tab who submits broken YAML must see a
// validation error (OutcomeDenied, invalidSpec toast copy),
// not a successful create with the (possibly stale) form
// fields — the handler differentiates based on the
// ErrInvalidYAMLSpec sentinel.
func TestBuildSpecFromRequestSurfacesYAMLError(t *testing.T) {
	t.Parallel()

	form := strings.NewReader("name=pg&kind=Postgres&spec_yaml=not%3A+%5Bvalid")

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/tenants/ns/apps", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if parseErr := req.ParseForm(); parseErr != nil {
		t.Fatalf("ParseForm: %v", parseErr)
	}

	pgh := &PageHandler{}

	_, err := pgh.buildSpecFromRequest(req, nil, "Postgres")
	if err == nil {
		t.Fatal("expected error from buildSpecFromRequest on malformed YAML")
	}

	if !errors.Is(err, ErrInvalidYAMLSpec) {
		t.Errorf("err = %v, want wraps ErrInvalidYAMLSpec", err)
	}
}

// TestBuildSpecFromRequestEmptyYAMLOnYAMLTab pins the
// defensive error path: the user is explicitly on the YAML
// tab but the textarea is empty. The server MUST refuse
// rather than silently fall through to the form pane and
// apply hidden form values. Without this check a user who
// cleared the YAML before pressing Save would see the create
// succeed with stale / default form values they never saw on
// screen.
func TestBuildSpecFromRequestEmptyYAMLOnYAMLTab(t *testing.T) {
	t.Parallel()

	form := strings.NewReader("_tabmode=yaml&name=pg&kind=Postgres&replicas=99&spec_yaml=")

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/tenants/ns/apps", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if parseErr := req.ParseForm(); parseErr != nil {
		t.Fatalf("ParseForm: %v", parseErr)
	}

	pgh := &PageHandler{}

	_, err := pgh.buildSpecFromRequest(req, nil, "Postgres")
	if err == nil {
		t.Fatal("expected ErrEmptyYAMLSpec on empty YAML while _tabmode=yaml")
	}

	if !errors.Is(err, ErrEmptyYAMLSpec) {
		t.Errorf("err = %v, want wraps ErrEmptyYAMLSpec", err)
	}
}

// TestExtractSpecFromFormSkipsTabMode pins the reserved-field
// contract: the _tabmode radio that drives the Form/YAML tab
// switch is UI state, not part of the CRD spec. Without this
// check extractSpecFromForm would surface `_tabmode: "form"`
// into the spec map — either rejected by the apiserver on a
// strict CRD or silently persisted as garbage.
func TestExtractSpecFromFormSkipsTabMode(t *testing.T) {
	t.Parallel()

	form := strings.NewReader("name=pg&kind=Postgres&_tabmode=form&replicas=3")

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/tenants/ns/apps", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if parseErr := req.ParseForm(); parseErr != nil {
		t.Fatalf("ParseForm: %v", parseErr)
	}

	spec := extractSpecFromForm(req, map[string]string{"replicas": "integer"})

	if _, ok := spec["_tabmode"]; ok {
		t.Errorf("_tabmode leaked into spec: %+v", spec)
	}

	if _, ok := spec["spec_yaml"]; ok {
		t.Errorf("spec_yaml leaked into spec: %+v", spec)
	}

	if spec["replicas"] != int64(3) {
		t.Errorf("replicas = %v, want 3", spec["replicas"])
	}
}
