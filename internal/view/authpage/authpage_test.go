package authpage

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/a-h/templ"
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
