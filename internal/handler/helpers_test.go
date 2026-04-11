package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lexfrei/cozytempl/internal/k8s"
)

func newFormRequest(t *testing.T, body string) *http.Request {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), "POST", "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	parseErr := req.ParseForm()
	if parseErr != nil {
		t.Fatalf("ParseForm: %v", parseErr)
	}

	return req
}

// TestExtractSpecFromFormReturnsEmptyNotNil locks in the bug-hunter P2 fix:
// a form with no schema fields must still produce an empty map so downstream
// CRD validators see a present-but-empty spec rather than a nil that some
// schemas reject.
func TestExtractSpecFromFormReturnsEmptyNotNil(t *testing.T) {
	t.Parallel()

	req := newFormRequest(t, "name=my-app&kind=Redis")

	spec := extractSpecFromForm(req, map[string]string{})

	if spec == nil {
		t.Fatal("extractSpecFromForm returned nil on empty spec; expected empty map")
	}
	if len(spec) != 0 {
		t.Errorf("extractSpecFromForm = %v, want empty map", spec)
	}
}

// TestExtractSpecFromFormSkipsNameKind makes sure the reserved form keys
// (name + kind) are not mirrored into the spec — they belong on
// metadata.name and metadata.kind, not spec.
func TestExtractSpecFromFormSkipsNameKind(t *testing.T) {
	t.Parallel()

	req := newFormRequest(t, "name=my-app&kind=Redis&replicas=3&tls=true")

	spec := extractSpecFromForm(req, map[string]string{
		"replicas": "integer",
		"tls":      "boolean",
	})

	if _, ok := spec["name"]; ok {
		t.Error("spec should not include reserved 'name' key")
	}
	if _, ok := spec["kind"]; ok {
		t.Error("spec should not include reserved 'kind' key")
	}

	if val, ok := spec["replicas"]; !ok || val != int64(3) {
		t.Errorf("spec.replicas = %v (%T), want int64(3)", val, val)
	}
	if val, ok := spec["tls"]; !ok || val != true {
		t.Errorf("spec.tls = %v, want true", val)
	}
}

// TestExtractTenantSpecSkipsParent verifies the 'parent' form field is not
// mirrored into spec — it's a metadata hint, not a spec field.
func TestExtractTenantSpecSkipsParent(t *testing.T) {
	t.Parallel()

	req := newFormRequest(t, "name=demo&parent=tenant-root&host=demo.example.com")

	spec := extractTenantSpec(req, map[string]string{"host": "string"})

	if _, ok := spec["parent"]; ok {
		t.Error("spec should not include 'parent' key")
	}
	if _, ok := spec["name"]; ok {
		t.Error("spec should not include 'name' key")
	}
	if val, ok := spec["host"]; !ok || val != "demo.example.com" {
		t.Errorf("spec.host = %v, want 'demo.example.com'", val)
	}
}

// TestConvertValueTypes covers the small type-coercion helper that turns
// raw form strings into the Go values CRD schemas expect. A nil or empty
// fieldType means "string".
func TestConvertValueTypes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw, typ string
		want     any
	}{
		{"true", "boolean", true},
		{"false", "boolean", false},
		{"0", "boolean", false},
		{"42", "integer", int64(42)},
		{"not-a-number", "integer", "not-a-number"}, // parse fail → raw string
		{"3.14", "number", 3.14},
		{"bad", "number", "bad"},
		{"hello", "string", "hello"},
		{"hello", "", "hello"},
	}

	for _, tc := range cases {
		t.Run(tc.typ+"/"+tc.raw, func(t *testing.T) {
			t.Parallel()

			got := convertValue(tc.raw, tc.typ)
			if got != tc.want {
				t.Errorf("convertValue(%q, %q) = %v (%T), want %v (%T)",
					tc.raw, tc.typ, got, got, tc.want, tc.want)
			}
		})
	}
}

// TestValidTenantName locks in cozystack 1.2's stricter tenant-name
// rule: lowercase alphanumerics only, starting with a letter, no
// dashes — because the downstream Helm chart composes release names
// as "tenant-<name>" and rejects any further dashes in the result.
// The function also enforces maxTenantNameLength to keep the final
// namespace under the DNS-1123 63-char limit.
func TestValidTenantName(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"":                      false, // empty
		"valid":                 true,
		"also123":               true,
		"UPPERCASE":             false,
		"has_underscore":        false,
		"has-dash":              false, // rejected by cozystack 1.2 chart
		"1leading":              false, // must start with a letter
		"has.dot":               false,
		"foo,injection=x":       false, // label-selector injection guard
		strings.Repeat("a", 56): true,  // exact cap
		strings.Repeat("a", 57): false, // over cap
	}

	for name, want := range cases {
		if got := validTenantName(name); got != want {
			t.Errorf("validTenantName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestExtractFieldTypes walks a fake JSON schema and pulls out property
// types, including the one-level-deep recursion into nested object
// properties. Deep (level 2+) objects and arrays are skipped to match
// the form renderer's depth cap — both walkers must produce the same
// set of form fields.
func TestExtractFieldTypes(t *testing.T) {
	t.Parallel()

	schema := &k8s.AppSchema{
		JSONSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"replicas": map[string]any{"type": "integer"},
				"tls":      map[string]any{"type": "boolean"},
				"host":     map[string]any{"type": "string"},
				"backup": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"enabled":  map[string]any{"type": "boolean"},
						"schedule": map[string]any{"type": "string"},
					},
				},
				"tags": map[string]any{"type": "array"}, // skipped
			},
		},
	}

	got := extractFieldTypes(schema)

	want := map[string]string{
		"replicas":        "integer",
		"tls":             "boolean",
		"host":            "string",
		"backup.enabled":  "boolean",
		"backup.schedule": "string",
	}

	for key, val := range want {
		if got[key] != val {
			t.Errorf("extractFieldTypes[%q] = %q, want %q", key, got[key], val)
		}
	}

	if _, present := got["tags"]; present {
		t.Errorf("extractFieldTypes should skip array types, got %q", got["tags"])
	}
	if _, present := got["backup"]; present {
		t.Errorf("extractFieldTypes should not emit the nested object itself, only its leaves")
	}
}

// TestExtractFieldTypesNilSchema makes sure the helper does not panic on
// a nil schema or a schema with a non-object JSONSchema shape.
func TestExtractFieldTypesNilSchema(t *testing.T) {
	t.Parallel()

	if got := extractFieldTypes(nil); len(got) != 0 {
		t.Errorf("extractFieldTypes(nil) = %v, want empty", got)
	}

	if got := extractFieldTypes(&k8s.AppSchema{}); len(got) != 0 {
		t.Errorf("extractFieldTypes(empty schema) = %v, want empty", got)
	}

	if got := extractFieldTypes(&k8s.AppSchema{JSONSchema: "not a map"}); len(got) != 0 {
		t.Errorf("extractFieldTypes(bad shape) = %v, want empty", got)
	}
}

// TestFilterAndSortAppsByQuery exercises the fragment filter/sort helper
// used by the live app-table refresh. The query filter is case-insensitive
// and matches on name substring.
func TestFilterAndSortAppsByQuery(t *testing.T) {
	t.Parallel()

	apps := []k8s.Application{
		{Name: "redis-primary", Kind: "Redis", Status: k8s.AppStatusReady},
		{Name: "postgres-main", Kind: "Postgres", Status: k8s.AppStatusReady},
		{Name: "redis-cache", Kind: "Redis", Status: k8s.AppStatusFailed},
		{Name: "nginx", Kind: "Ingress", Status: k8s.AppStatusReady},
	}

	filtered := filterAndSortApps(apps, "redis", "", "name")

	if len(filtered) != 2 {
		t.Fatalf("query 'redis' matched %d apps, want 2", len(filtered))
	}
	if filtered[0].Name != "redis-cache" || filtered[1].Name != "redis-primary" {
		t.Errorf("sort order wrong: %v", []string{filtered[0].Name, filtered[1].Name})
	}
}

// TestFilterAndSortAppsByKind verifies the kind filter is exact-match.
func TestFilterAndSortAppsByKind(t *testing.T) {
	t.Parallel()

	apps := []k8s.Application{
		{Name: "a", Kind: "Redis"},
		{Name: "b", Kind: "Postgres"},
		{Name: "c", Kind: "Redis"},
	}

	filtered := filterAndSortApps(apps, "", "Redis", "name")

	if len(filtered) != 2 {
		t.Fatalf("kind filter matched %d apps, want 2", len(filtered))
	}
	if filtered[0].Name != "a" || filtered[1].Name != "c" {
		t.Errorf("unexpected apps: %v", filtered)
	}
}

// TestFilterAndSortAppsSortOrder covers the three sort modes (name default,
// kind, status).
func TestFilterAndSortAppsSortOrder(t *testing.T) {
	t.Parallel()

	apps := []k8s.Application{
		{Name: "charlie", Kind: "Postgres", Status: k8s.AppStatusFailed},
		{Name: "alpha", Kind: "Redis", Status: k8s.AppStatusReady},
		{Name: "bravo", Kind: "Redis", Status: k8s.AppStatusReconciling},
	}

	byName := filterAndSortApps(apps, "", "", "name")
	if byName[0].Name != "alpha" || byName[2].Name != "charlie" {
		t.Errorf("sort by name: %v", byName)
	}

	byKind := filterAndSortApps(apps, "", "", "kind")
	if byKind[0].Kind != "Postgres" {
		t.Errorf("sort by kind: %v", byKind)
	}

	byStatus := filterAndSortApps(apps, "", "", "status")
	if byStatus[0].Status != k8s.AppStatusFailed {
		t.Errorf("sort by status: %v", byStatus)
	}
}

// TestValidAppName is the server-side safety net for the
// <input pattern="..."> regex in the create-application form. Any
// change to one must match the other, otherwise the UI and the
// server diverge and the user sees the generic "Failed to create"
// error toast instead of a precise message.
func TestValidAppName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"empty rejected", "", false},
		{"simple ok", "my-app", true},
		{"alphanum only ok", "redis1", true},
		{"single char ok", "a", true},
		{"leading hyphen rejected", "-foo", false},
		{"trailing hyphen rejected", "foo-", false},
		{"uppercase rejected", "MyApp", false},
		{"underscore rejected", "my_app", false},
		{"dot rejected", "my.app", false},
		{"space rejected", "my app", false},
		{"max length ok", strings.Repeat("a", 53), true},
		{"over max length rejected", strings.Repeat("a", 54), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validAppName(tt.input)
			if got != tt.want {
				t.Errorf("validAppName(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
