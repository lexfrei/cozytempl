package authpage

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/lexfrei/cozytempl/static"
)

// TestAuthPagesLinkCorrectStylesheet pins both auth templates
// to the path that actually exists in the static embed. An
// earlier revision shipped `/static/dist/styles.css`, which
// 404s — the file lives under `/static/css/styles.css`. The
// templates rendered with browser defaults (Times New Roman,
// native form controls) and operators running demos fell back
// to `dev` mode with a cluster-admin binding instead of the
// token / byok modes cozytempl was designed to ship in.
//
// Double guard: (1) the correct path appears, (2) the wrong
// path does not leak back in via a copy-paste regression.
func TestAuthPagesLinkCorrectStylesheet(t *testing.T) {
	t.Parallel()

	cases := map[string]templ.Component{
		"token":      TokenUpload(""),
		"kubeconfig": KubeconfigUpload(""),
	}

	for name, component := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := renderToString(component)
			if err != nil {
				t.Fatalf("render: %v", err)
			}

			if !strings.Contains(got, `href="/static/css/styles.css?v=`) {
				t.Errorf("%s page did not link /static/css/styles.css (with cache buster):\n%s",
					name, got)
			}

			if strings.Contains(got, `/static/dist/styles.css`) {
				t.Errorf("%s page still links the wrong /static/dist/styles.css path:\n%s",
					name, got)
			}
		})
	}
}

func renderToString(c templ.Component) (string, error) {
	var b strings.Builder

	err := c.Render(context.Background(), &b)
	if err != nil {
		return "", fmt.Errorf("render templ: %w", err)
	}

	return b.String(), nil
}

// staticRefPattern captures every /static/... reference in
// rendered HTML. The capture group grabs the path up to the
// query string; `?v=` cache busters are stripped before the
// file-existence check.
var staticRefPattern = regexp.MustCompile(`/static/([^"'?\s]+)`)

// TestAuthPagesStaticRefsExist is the class-defence guard
// this file exists for: every /static/... reference emitted by
// an auth page must resolve to a real file inside the embedded
// FS. A future copy-paste of the wrong path (dist/styles.css,
// css/bundle.js, etc.) breaks this test before it reaches a
// user-facing demo. Captures paths straight from the rendered
// HTML so no hand-maintained allowlist can drift.
func TestAuthPagesStaticRefsExist(t *testing.T) {
	t.Parallel()

	pages := map[string]templ.Component{
		"token":      TokenUpload(""),
		"kubeconfig": KubeconfigUpload(""),
	}

	for name, component := range pages {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := renderToString(component)
			if err != nil {
				t.Fatalf("render: %v", err)
			}

			matches := staticRefPattern.FindAllStringSubmatch(got, -1)
			if len(matches) == 0 {
				t.Fatalf("%s page has no /static/ references — did the stylesheet <link> move?",
					name)
			}

			for _, m := range matches {
				path := m[1]

				// Does the file exist inside the embedded FS?
				// static.FS is rooted at the repo's static/
				// directory, so the leading "/static/" slice
				// belongs to the URL space, not the FS path.
				f, openErr := static.FS.Open(path)
				if openErr != nil {
					t.Errorf("%s page references /static/%s but the file is not in the embedded FS: %v",
						name, path, openErr)

					continue
				}

				_ = f.Close()
			}
		})
	}
}
