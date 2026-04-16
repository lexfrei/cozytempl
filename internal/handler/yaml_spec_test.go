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

	if !strings.Contains(err.Error(), "parsing spec yaml") {
		t.Errorf("err = %v, want wrapped 'parsing spec yaml' prefix", err)
	}
}

// TestBuildSpecFromRequestPrefersYAML pins the "YAML wins when
// non-empty" rule. Form fields are present but spec_yaml is
// also set — the server must pick the YAML source so a user
// who switches to the YAML tab and submits bypasses the
// schema-driven form entirely.
func TestBuildSpecFromRequestPrefersYAML(t *testing.T) {
	t.Parallel()

	form := strings.NewReader("name=pg&kind=Postgres&replicas=99&spec_yaml=replicas%3A%203")

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/tenants/ns/apps", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if parseErr := req.ParseForm(); parseErr != nil {
		t.Fatalf("ParseForm: %v", parseErr)
	}

	// buildSpecFromRequest hits pgh.schemaSvc.Get on the form
	// branch; for the YAML branch it short-circuits before
	// touching any dependency, so a nil handler is safe here.
	pgh := &PageHandler{}

	spec, err := pgh.buildSpecFromRequest(req, nil, "Postgres")
	if err != nil {
		t.Fatalf("buildSpecFromRequest: %v", err)
	}

	switch v := spec["replicas"].(type) {
	case float64:
		if v != 3 {
			t.Errorf("replicas = %v, want 3 from YAML (not 99 from form)", v)
		}
	case int64:
		if v != 3 {
			t.Errorf("replicas = %v, want 3 from YAML (not 99 from form)", v)
		}
	default:
		t.Errorf("replicas wrong type: %T", spec["replicas"])
	}

	if _, ok := spec["kind"]; ok {
		t.Error("kind leaked into spec from form; YAML branch should ignore reserved form fields")
	}
}

// TestBuildSpecFromRequestSurfacesYAMLError pins that a bad
// spec_yaml does not silently fall through to the form branch.
// A user on the YAML tab who submits broken YAML must see a
// validation error, not a successful create with the
// (possibly stale) form fields.
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

	if _, err := pgh.buildSpecFromRequest(req, nil, "Postgres"); !errors.Is(err, err) || err == nil {
		t.Fatal("expected error from buildSpecFromRequest on malformed YAML")
	}
}
