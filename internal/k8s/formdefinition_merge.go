package k8s

// OverridesByPath folds a slice of FormFieldOverride entries
// into a path→override lookup. Later entries win on path
// conflict, matching the "last FormDefinition in name order
// wins" contract documented on FormDefinitionService.
//
// Returned map is nil when the input is empty, to keep the
// render layer's branch (nil-map skip-merge) lean.
func OverridesByPath(overrides []FormFieldOverride) map[string]FormFieldOverride {
	if len(overrides) == 0 {
		return nil
	}

	result := make(map[string]FormFieldOverride, len(overrides))

	for _, o := range overrides {
		if o.Path == "" {
			continue
		}

		result[o.Path] = o
	}

	return result
}

// ApplyLabelOverride returns the rendered label for a field
// at path. If an override defines Label the override wins;
// otherwise the schema-derived fallback is returned verbatim.
// Kept tiny + pure so the render-layer tests can exercise the
// contract without constructing the whole templ tree.
func ApplyLabelOverride(overrides map[string]FormFieldOverride, path, fallback string) string {
	o, ok := overrides[path]
	if !ok || o.Label == "" {
		return fallback
	}

	return o.Label
}

// ApplyHintOverride is the description/hint counterpart of
// ApplyLabelOverride. Same "override-wins-when-non-empty"
// semantics.
func ApplyHintOverride(overrides map[string]FormFieldOverride, path, fallback string) string {
	o, ok := overrides[path]
	if !ok || o.Hint == "" {
		return fallback
	}

	return o.Hint
}

// ApplyPlaceholderOverride returns the placeholder string for
// a field at path, or the fallback (typically empty) when no
// override is in effect. The schema itself does not emit a
// placeholder today, so the fallback is usually "" and the
// override controls whether the <input placeholder=…>
// attribute is rendered at all.
func ApplyPlaceholderOverride(overrides map[string]FormFieldOverride, path, fallback string) string {
	o, ok := overrides[path]
	if !ok || o.Placeholder == "" {
		return fallback
	}

	return o.Placeholder
}

// IsHidden reports whether the path should be dropped from
// the rendered form. A nil/missing override returns false,
// preserving the pre-existing behaviour of rendering every
// schema field.
func IsHidden(overrides map[string]FormFieldOverride, path string) bool {
	o, ok := overrides[path]
	if !ok {
		return false
	}

	return o.Hidden
}

// OrderFor returns the render order for a field. The second
// return is false when no explicit order was set; callers
// should sort unordered fields by the schema's natural sort
// key (alphabetical), which keeps fields with no override
// stable relative to one another and after any ordered
// fields.
func OrderFor(overrides map[string]FormFieldOverride, path string) (int, bool) {
	o, ok := overrides[path]
	if !ok || o.Order == nil {
		return 0, false
	}

	return *o.Order, true
}
