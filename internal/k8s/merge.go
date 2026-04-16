package k8s

// pickNextSpec decides how a new spec replaces the existing
// one on Update. Extracted from ApplicationService.Update so
// the dispatch is exercisable by a pure-function test without
// the whole fake-dynamic-client stack.
//
//   - replace == true  → incoming wins verbatim, keys absent
//     from incoming are deleted from cluster state. This is
//     what a YAML-editor user expects after running
//     `kubectl edit` (deleted line = deleted field).
//   - replace == false → deep-merge so a partial form submit
//     that only set a couple of fields does not silently
//     drop the rest of the existing spec.
func pickNextSpec(existing, incoming map[string]any, replace bool) map[string]any {
	if replace {
		return incoming
	}

	return deepMergeSpec(existing, incoming)
}

// deepMergeSpec merges incoming into base in place and returns base.
// Nested maps are merged recursively so a partial update that only
// touches spec.backup.schedule does not clobber the sibling keys
// spec.backup.s3SecretKey, spec.backup.retentionPolicy, etc.
//
// Semantics:
//   - Scalar / array / non-map values are replaced as-is.
//   - Map values recurse into deepMergeSpec(existing, new).
//   - Keys present only in base are preserved.
//   - Keys present only in incoming are added.
//   - If the type changes (map <-> scalar), incoming wins — this is
//     what a user who just picked a different enum value would expect.
//
// Used by every *Service.Update path so partial form submissions in
// the UI don't silently drop the fields the user didn't touch.
func deepMergeSpec(base, incoming map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}

	for key, newVal := range incoming {
		existing, present := base[key]
		if !present {
			base[key] = newVal

			continue
		}

		baseMap, baseIsMap := existing.(map[string]any)
		newMap, newIsMap := newVal.(map[string]any)

		if baseIsMap && newIsMap {
			base[key] = deepMergeSpec(baseMap, newMap)

			continue
		}

		base[key] = newVal
	}

	return base
}
