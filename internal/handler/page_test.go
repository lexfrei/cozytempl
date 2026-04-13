package handler

import (
	"testing"

	"github.com/lexfrei/cozytempl/internal/k8s"
)

// TestSelectKnownKind is the single gate that keeps user-controlled
// query params from flowing into rendered URLs and the create-app
// modal. Only values that exactly match a known AppSchema kind pass
// through; anything else — empty, unknown, injection-crafted —
// collapses to "".
func TestSelectKnownKind(t *testing.T) {
	t.Parallel()

	schemas := []k8s.AppSchema{
		{Kind: "Etcd"},
		{Kind: "Redis"},
	}

	cases := map[string]string{
		"":              "",     // empty stays empty
		"Etcd":          "Etcd", // exact match accepted
		"Redis":         "Redis",
		"Postgres":      "", // unknown kind rejected
		"Etcd&evil=1":   "", // URL injection rejected
		"etcd":          "", // case-sensitive
		"Etcd ":         "", // no trimming
		" Etcd":         "", // no trimming
		"<script>":      "", // HTML payload rejected
		"Etcd/../admin": "", // path traversal rejected
	}

	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			got := selectKnownKind(input, schemas)
			if got != want {
				t.Errorf("selectKnownKind(%q, schemas) = %q, want %q", input, got, want)
			}
		})
	}
}

// TestSelectKnownKindEmptySchemas makes sure the function does not
// panic on a nil / empty schema list — schemaSvc.List errors are
// logged and dropped in the caller, so an empty slice is a real code
// path, not just a test fixture.
func TestSelectKnownKindEmptySchemas(t *testing.T) {
	t.Parallel()

	if got := selectKnownKind("Etcd", nil); got != "" {
		t.Errorf("selectKnownKind(%q, nil) = %q, want empty", "Etcd", got)
	}

	if got := selectKnownKind("Etcd", []k8s.AppSchema{}); got != "" {
		t.Errorf("selectKnownKind(%q, []) = %q, want empty", "Etcd", got)
	}
}
