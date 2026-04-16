package k8s

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestOverridesByPathLastWriteWins(t *testing.T) {
	t.Parallel()

	// Two entries at the same path; the second must win.
	// Mirrors the "later FormDefinition in name order wins"
	// contract the service layer documents — a regression
	// that returned the first would silently flip the
	// precedence and break operators' layered overlays
	// (base + tenant-scoped refinement).
	overrides := []FormFieldOverride{
		{Path: "replicas", Label: "base"},
		{Path: "replicas", Label: "override"},
	}

	out := OverridesByPath(overrides)

	if got := out["replicas"].Label; got != "override" {
		t.Errorf("replicas label = %q, want override", got)
	}
}

func TestOverridesByPathSkipsEmptyPath(t *testing.T) {
	t.Parallel()

	// An override with empty Path is a malformed CR entry
	// (would cover every unnamed field). Parse layer
	// already rejects these but defence in depth matters —
	// a future caller constructing the slice by hand must
	// not map an empty key and silently mask every field.
	overrides := []FormFieldOverride{
		{Path: "", Label: "oops"},
		{Path: "replicas", Label: "ok"},
	}

	out := OverridesByPath(overrides)

	if _, leaked := out[""]; leaked {
		t.Error("empty-path override leaked into the map")
	}

	if got := out["replicas"].Label; got != "ok" {
		t.Errorf("replicas label = %q, want ok", got)
	}
}

func TestOverridesByPathEmptyInputReturnsNil(t *testing.T) {
	t.Parallel()

	// Nil return is the contract: the render layer uses
	// `if overrides == nil` as the short-circuit path, and
	// an empty map costs more to carry around than the
	// absence.
	if got := OverridesByPath(nil); got != nil {
		t.Errorf("nil input should return nil, got %v", got)
	}

	if got := OverridesByPath([]FormFieldOverride{}); got != nil {
		t.Errorf("empty input should return nil, got %v", got)
	}
}

func TestApplyLabelOverride(t *testing.T) {
	t.Parallel()

	overrides := OverridesByPath([]FormFieldOverride{
		{Path: "replicas", Label: "Replica Count"},
		{Path: "storage.size", Label: ""},
	})

	cases := []struct {
		name     string
		path     string
		fallback string
		want     string
	}{
		{"override wins", "replicas", "Replicas", "Replica Count"},
		{"empty-label override falls back", "storage.size", "Size", "Size"},
		{"no override at all falls back", "unknown", "Unknown", "Unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := ApplyLabelOverride(overrides, tc.path, tc.fallback); got != tc.want {
				t.Errorf("ApplyLabelOverride(%q, %q) = %q, want %q",
					tc.path, tc.fallback, got, tc.want)
			}
		})
	}
}

func TestApplyHintOverride(t *testing.T) {
	t.Parallel()

	overrides := OverridesByPath([]FormFieldOverride{
		{Path: "replicas", Hint: "Number of pods running the workload."},
	})

	if got := ApplyHintOverride(overrides, "replicas", "schema desc"); got != "Number of pods running the workload." {
		t.Errorf("ApplyHintOverride: got %q, want override", got)
	}

	if got := ApplyHintOverride(overrides, "missing", "schema desc"); got != "schema desc" {
		t.Errorf("ApplyHintOverride: got %q, want schema desc fallback", got)
	}
}

func TestApplyPlaceholderOverride(t *testing.T) {
	t.Parallel()

	overrides := OverridesByPath([]FormFieldOverride{
		{Path: "replicas", Placeholder: "e.g. 3"},
	})

	if got := ApplyPlaceholderOverride(overrides, "replicas", ""); got != "e.g. 3" {
		t.Errorf("ApplyPlaceholderOverride: got %q, want override", got)
	}

	// No override → fallback stays empty (default behaviour
	// is no placeholder attribute on the input).
	if got := ApplyPlaceholderOverride(overrides, "missing", ""); got != "" {
		t.Errorf("ApplyPlaceholderOverride on missing: got %q, want empty", got)
	}
}

func TestIsHidden(t *testing.T) {
	t.Parallel()

	overrides := OverridesByPath([]FormFieldOverride{
		{Path: "internal.debug", Hidden: true},
		{Path: "replicas", Hidden: false},
	})

	if !IsHidden(overrides, "internal.debug") {
		t.Error("internal.debug should be hidden")
	}

	if IsHidden(overrides, "replicas") {
		t.Error("replicas explicitly Hidden=false should not be hidden")
	}

	if IsHidden(overrides, "missing") {
		t.Error("missing path should default to visible")
	}

	// nil map also defaults to visible; production renders
	// without any FormDefinition hit this path.
	if IsHidden(nil, "replicas") {
		t.Error("nil overrides map should default to visible")
	}
}

func TestOrderFor(t *testing.T) {
	t.Parallel()

	explicit := 3
	zero := 0

	overrides := OverridesByPath([]FormFieldOverride{
		{Path: "replicas", Order: &explicit},
		{Path: "banner", Order: &zero},
		{Path: "unordered"},
	})

	if order, ok := OrderFor(overrides, "replicas"); !ok || order != 3 {
		t.Errorf("OrderFor(replicas) = (%d, %v), want (3, true)", order, ok)
	}

	// Zero is a valid explicit order — pointer differentiates
	// "set to 0" from "not set". Without pointers the render
	// layer could not distinguish the two.
	if order, ok := OrderFor(overrides, "banner"); !ok || order != 0 {
		t.Errorf("OrderFor(banner) = (%d, %v), want (0, true); zero must be a valid explicit order", order, ok)
	}

	if _, ok := OrderFor(overrides, "unordered"); ok {
		t.Errorf("OrderFor(unordered) returned ok=true; unset order must report !ok")
	}

	if _, ok := OrderFor(overrides, "missing"); ok {
		t.Errorf("OrderFor(missing) returned ok=true; absent override must report !ok")
	}
}

// TestParseFieldOverrideCoercesOrderTypes exercises the
// unstructured-decode side of the parser: integers may arrive
// as int64 (sigs.k8s.io/yaml integer path) or float64 (a
// value like "3.0" or a JSON-numbers-always-float64 client).
// The parser must coerce both into a concrete int so
// OrderFor returns the same render order regardless of which
// client applied the CRD.
func TestParseFieldOverrideCoercesOrderTypes(t *testing.T) {
	t.Parallel()

	cases := map[string]any{
		"int64":   int64(5),
		"float64": float64(5),
		"int":     5,
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := parseFieldOverride(map[string]any{
				"path":  "replicas",
				"order": raw,
			})

			if got == nil {
				t.Fatal("parseFieldOverride returned nil for a well-formed entry")
			}

			if got.Order == nil {
				t.Fatalf("parseFieldOverride dropped order from %T input", raw)
			}

			if *got.Order != 5 {
				t.Errorf("parseFieldOverride(%T) order = %d, want 5", raw, *got.Order)
			}
		})
	}
}

func TestParseFieldOverrideRejectsEmptyPath(t *testing.T) {
	t.Parallel()

	got := parseFieldOverride(map[string]any{"path": ""})
	if got != nil {
		t.Error("parseFieldOverride accepted empty path; malformed entries must be dropped")
	}
}

// TestFormDefinitionsFromListPrecedence pins the "later name
// in sort order wins on path conflict" contract that the whole
// merge layer depends on. If this flips, operators who layer
// overlays like "00-base" + "50-tenant-a-overrides" get the
// wrong overlay rendered.
func TestFormDefinitionsFromListPrecedence(t *testing.T) {
	t.Parallel()

	items := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"name": "50-override"},
			"spec": map[string]any{
				"kind": "Postgres",
				"fields": []any{
					map[string]any{"path": "replicas", "label": "Override"},
				},
			},
		}},
		{Object: map[string]any{
			"metadata": map[string]any{"name": "00-base"},
			"spec": map[string]any{
				"kind": "Postgres",
				"fields": []any{
					map[string]any{"path": "replicas", "label": "Base"},
				},
			},
		}},
	}

	defs := formDefinitionsFromList(items)
	if len(defs) != 2 {
		t.Fatalf("got %d defs, want 2", len(defs))
	}

	// Sort is on metadata.name alphabetical; "00-base" sorts
	// first, "50-override" second.
	if defs[0].Fields[0].Label != "Base" {
		t.Errorf("defs[0] = %q, want Base — sort-by-name broken", defs[0].Fields[0].Label)
	}

	if defs[1].Fields[0].Label != "Override" {
		t.Errorf("defs[1] = %q, want Override — sort-by-name broken", defs[1].Fields[0].Label)
	}

	// Fold the slice through OverridesByPath: last-write-wins
	// means "50-override" must be the effective label.
	var all []FormFieldOverride
	for _, d := range defs {
		all = append(all, d.Fields...)
	}

	merged := OverridesByPath(all)
	if merged["replicas"].Label != "Override" {
		t.Errorf("merged label = %q, want Override (last-write-wins on path conflict)", merged["replicas"].Label)
	}
}

// TestFormDefinitionsFromListSkipsMalformed confirms that a
// single malformed CR in the list does not taint every other
// overlay. An operator who applies a FormDefinition with a
// missing spec.kind should see their own CR ignored, not every
// sibling FormDefinition silently dropped.
func TestFormDefinitionsFromListSkipsMalformed(t *testing.T) {
	t.Parallel()

	items := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"name": "good"},
			"spec": map[string]any{
				"kind": "Postgres",
				"fields": []any{
					map[string]any{"path": "replicas", "label": "OK"},
				},
			},
		}},
		{Object: map[string]any{
			"metadata": map[string]any{"name": "bad-missing-kind"},
			"spec":     map[string]any{},
		}},
		{Object: map[string]any{
			"metadata": map[string]any{"name": "bad-no-spec"},
		}},
	}

	defs := formDefinitionsFromList(items)
	if len(defs) != 1 {
		t.Fatalf("got %d defs, want 1 (the good one)", len(defs))
	}

	if defs[0].Kind != "Postgres" {
		t.Errorf("surviving def kind = %q, want Postgres", defs[0].Kind)
	}
}

// TestParseFormDefinitionMissingSpecKind pins the specific
// error the CRD's schema also enforces: spec.kind is required.
// A CR without it is rejected as ErrInvalidFormDefinition so
// the higher-level list loop can log + skip.
func TestParseFormDefinitionMissingSpecKind(t *testing.T) {
	t.Parallel()

	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "no-kind"},
		"spec":     map[string]any{"kind": ""},
	}}

	_, err := parseFormDefinition(obj)
	if err == nil {
		t.Fatal("parseFormDefinition accepted empty spec.kind")
	}

	if !errors.Is(err, ErrInvalidFormDefinition) {
		t.Errorf("err = %v, want wraps ErrInvalidFormDefinition", err)
	}
}
