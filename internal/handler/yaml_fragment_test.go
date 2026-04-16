package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
)

// fakeSchemaService is the narrow schemaService stub the
// fragment handler tests thread into PageHandler. Get returns a
// canned schema keyed by kind; List returns the values. An err
// lets a test exercise the 404 branch without spinning up a
// real SchemaService cache.
type fakeSchemaService struct {
	schemas map[string]*k8s.AppSchema
	err     error
}

func (f *fakeSchemaService) Get(_ context.Context, _ *auth.UserContext, kind string) (*k8s.AppSchema, error) {
	if f.err != nil {
		return nil, f.err
	}

	s, ok := f.schemas[kind]
	if !ok {
		return nil, errors.New("not found")
	}

	return s, nil
}

func (f *fakeSchemaService) List(_ context.Context, _ *auth.UserContext) ([]k8s.AppSchema, error) {
	if f.err != nil {
		return nil, f.err
	}

	out := make([]k8s.AppSchema, 0, len(f.schemas))
	for _, s := range f.schemas {
		out = append(out, *s)
	}

	return out, nil
}

// fakePostgresSchema returns a minimal-but-realistic Postgres-
// like schema the handler tests use to exercise the form →
// YAML → form round-trip. Shape mirrors what
// extractFieldTypes reads: properties map with a few scalars
// and a nested object.
func fakePostgresSchema() *k8s.AppSchema {
	return &k8s.AppSchema{
		Kind: "Postgres",
		JSONSchema: map[string]any{
			"properties": map[string]any{
				"replicas": map[string]any{"type": "integer"},
				"enabled":  map[string]any{"type": "boolean"},
				"storage": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"size": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
}

// TestAppFormYAMLFragmentRequiresAuth pins the 401 guard — a
// misconfigured route without the middleware must not leak
// form-sourced spec YAML to an anonymous caller.
func TestAppFormYAMLFragmentRequiresAuth(t *testing.T) {
	t.Parallel()

	pgh := &PageHandler{log: slog.New(slog.DiscardHandler)}

	req := httptest.NewRequestWithContext(
		context.Background(), http.MethodPost, "/fragments/app-yaml", nil)

	rec := httptest.NewRecorder()
	pgh.AppFormYAMLFragment(rec, req)

	// requireUser renders an error page instead of writing
	// 401 — either a non-200 status or a redirect/HTML error
	// body proves the guard fired. Accept anything that's
	// clearly not a rendered YAML response.
	if rec.Code == http.StatusOK {
		t.Errorf("status = %d, want non-OK for anonymous caller", rec.Code)
	}
}

// TestAppFormYAMLFragmentEmptyKindReturnsBlank confirms the
// no-kind branch returns an empty textarea body rather than a
// 400 or a schema-fetch against "". The create modal renders
// the YAML tab before the user picks a Kind, so a GET with no
// kind is the expected first paint, not an error.
func TestAppFormYAMLFragmentEmptyKindReturnsBlank(t *testing.T) {
	t.Parallel()

	pgh := &PageHandler{log: slog.New(slog.DiscardHandler)}

	form := strings.NewReader("name=pg")
	req := httptest.NewRequestWithContext(
		withFragmentTestUser(t), http.MethodPost, "/fragments/app-yaml", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	pgh.AppFormYAMLFragment(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 on empty-kind branch", rec.Code)
	}

	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty on empty-kind", rec.Body.String())
	}
}

// TestAppFormYAMLToFormFragmentRequiresKind pins the 400
// guard for the apply-to-form endpoint: without a kind the
// server can't fetch a schema and can't render fields. The
// client-side hx-include is expected to ship kind every time;
// a missing value is a programming bug the handler should
// surface loudly.
func TestAppFormYAMLToFormFragmentRequiresKind(t *testing.T) {
	t.Parallel()

	pgh := &PageHandler{log: slog.New(slog.DiscardHandler)}

	form := strings.NewReader("spec_yaml=replicas%3A+3")
	req := httptest.NewRequestWithContext(
		withFragmentTestUser(t), http.MethodPost, "/fragments/app-yaml-to-form", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	pgh.AppFormYAMLToFormFragment(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 on missing kind", rec.Code)
	}
}

// withFragmentTestUser attaches a user to the request context
// so the requireUser guard in each fragment handler clears and
// the test can reach the code path under examination. The
// user identity itself is inert — no RBAC check runs before
// the branches these tests target.
func withFragmentTestUser(t *testing.T) context.Context {
	t.Helper()

	return auth.ContextWithUser(t.Context(),
		&auth.UserContext{Username: "test-user"})
}

// TestAppFormYAMLFragmentEscapesTextareaBreakout pins the
// html.EscapeString on the YAML preview output. The textarea
// the response is swapped into via hx-target is
// innerHTML-swapped by htmx; a user-supplied value that
// contains "</textarea>" or "<script>" would otherwise break
// out of the element and inject arbitrary HTML. CSP blocks
// the script from executing, but broken layout + phishing
// content still harm the user, so entity-encoding on the
// server is a load-bearing invariant.
func TestAppFormYAMLFragmentEscapesTextareaBreakout(t *testing.T) {
	t.Parallel()

	pgh := &PageHandler{
		log:       slog.New(slog.DiscardHandler),
		schemaSvc: &fakeSchemaService{schemas: map[string]*k8s.AppSchema{"Postgres": fakePostgresSchema()}},
	}

	// A user pastes a value containing "</textarea>" into a
	// string-typed form field. Without escaping, the resulting
	// YAML preview would close the textarea in the modal and
	// bleed the remaining spec into DOM siblings.
	form := strings.NewReader(
		"kind=Postgres&storage.size=10Gi%3C%2Ftextarea%3E%3Cscript%3Ealert(1)%3C%2Fscript%3E")
	req := httptest.NewRequestWithContext(
		withFragmentTestUser(t), http.MethodPost, "/fragments/app-yaml", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	pgh.AppFormYAMLFragment(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()

	if strings.Contains(body, "</textarea>") {
		t.Errorf("response leaked literal </textarea>; escape contract broken:\n%s", body)
	}

	if strings.Contains(body, "<script>") {
		t.Errorf("response leaked literal <script>; escape contract broken:\n%s", body)
	}

	// And the escaped form must be present — confirms the
	// value reached the output at all instead of being
	// silently dropped upstream.
	if !strings.Contains(body, "&lt;/textarea&gt;") {
		t.Errorf("escaped </textarea> not found in body; content may not have been rendered:\n%s", body)
	}
}

// TestAppFormYAMLToFormFragmentHappyPath pins two invariants
// at once: (1) a valid YAML body round-trips into rendered
// schema fields, and (2) the response MUST NOT wrap the
// output in a bare <div id="schema-fields">. That wrapper
// used to land in the DOM right next to the create modal's
// own #schema-fields, and htmx selectors that still targeted
// the bare id would silently pick the wrong modal (first
// document-order match). The outer scoped container
// id="<bodyID>-schema-fields" already exists at the target,
// so bare fields are all the handler should write.
func TestAppFormYAMLToFormFragmentHappyPath(t *testing.T) {
	t.Parallel()

	pgh := &PageHandler{
		log:       slog.New(slog.DiscardHandler),
		schemaSvc: &fakeSchemaService{schemas: map[string]*k8s.AppSchema{"Postgres": fakePostgresSchema()}},
	}

	form := strings.NewReader("kind=Postgres&spec_yaml=replicas%3A+7")
	req := httptest.NewRequestWithContext(
		withFragmentTestUser(t), http.MethodPost, "/fragments/app-yaml-to-form", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	pgh.AppFormYAMLToFormFragment(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()

	if strings.Contains(body, `id="schema-fields"`) {
		t.Errorf(
			"response wrapped output in bare <div id=\"schema-fields\">; collides with"+
				" sibling modals after Apply-to-Form:\n%s", body)
	}

	// The rendered form should carry the YAML-supplied
	// replicas value so the round-trip is actually
	// end-to-end verified, not just status + wrapper.
	if !strings.Contains(body, `value="7"`) {
		t.Errorf("replicas=7 not rendered into the form; round-trip broken:\n%s", body)
	}
}

// TestAppFormYAMLToFormFragmentParseFailureKeepsSchema pins
// the fallback path: when the user-typed YAML is
// un-parseable, the handler logs and renders an un-populated
// form anyway. A hard error here would wedge the modal with
// no recovery path. The user's YAML draft is preserved in
// the textarea because Apply-to-Form only targets the form
// pane container.
func TestAppFormYAMLToFormFragmentParseFailureKeepsSchema(t *testing.T) {
	t.Parallel()

	pgh := &PageHandler{
		log:       slog.New(slog.DiscardHandler),
		schemaSvc: &fakeSchemaService{schemas: map[string]*k8s.AppSchema{"Postgres": fakePostgresSchema()}},
	}

	// Malformed YAML: opening a sequence then a mapping
	// without a key. sigs.k8s.io/yaml rejects it.
	form := strings.NewReader("kind=Postgres&spec_yaml=%5Bnot%3A+valid")
	req := httptest.NewRequestWithContext(
		withFragmentTestUser(t), http.MethodPost, "/fragments/app-yaml-to-form", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	pgh.AppFormYAMLToFormFragment(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even on parse failure — the UI should never wedge", rec.Code)
	}

	body := rec.Body.String()

	// The schema fields are still rendered (the replicas
	// input is the sentinel). Without the schema, the modal
	// would be empty and the user would have no recovery
	// path short of closing and reopening.
	if !strings.Contains(body, `name="replicas"`) {
		t.Errorf("schema fields missing on parse-failure path; modal would wedge:\n%s", body)
	}
}

// TestAppFormYAMLFragmentSchemaErrorSurfacesAs404 pins the
// fail-closed branch: a schema-fetch error becomes a 404 so
// htmx stops swapping. The alternative (silently returning
// empty YAML) would leave the user with a suspiciously clean
// textarea and no signal that their cluster state was not
// actually read.
func TestAppFormYAMLFragmentSchemaErrorSurfacesAs404(t *testing.T) {
	t.Parallel()

	pgh := &PageHandler{
		log:       slog.New(slog.DiscardHandler),
		schemaSvc: &fakeSchemaService{err: errors.New("boom")},
	}

	form := strings.NewReader("kind=Postgres")
	req := httptest.NewRequestWithContext(
		withFragmentTestUser(t), http.MethodPost, "/fragments/app-yaml", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	pgh.AppFormYAMLFragment(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 on schema-fetch error", rec.Code)
	}
}

// TestMarshalSpecForEditEmpty pins the short-circuit: an
// empty spec returns "" without round-tripping through
// yaml.Marshal. Edit modals for apps with no spec would
// otherwise pre-populate the textarea with "{}\n", which a
// user who hit Save in YAML mode (replace semantics) would
// then write back to the cluster as an explicit empty spec.
// The "" sentinel is what AppFormTabs checks to skip
// seeding.
func TestMarshalSpecForEditEmpty(t *testing.T) {
	t.Parallel()

	pgh := &PageHandler{log: slog.New(slog.DiscardHandler)}

	got := pgh.marshalSpecForEdit("tenant-x", "app-y", map[string]any{})
	if got != "" {
		t.Errorf("marshalSpecForEdit(empty) = %q, want empty string", got)
	}

	got = pgh.marshalSpecForEdit("tenant-x", "app-y", nil)
	if got != "" {
		t.Errorf("marshalSpecForEdit(nil) = %q, want empty string", got)
	}
}

// TestMarshalSpecForEditHappyPath confirms a non-empty spec
// emerges as YAML text the textarea can display. Without
// this the edit-modal YAML tab would silently open blank
// and a Save-in-YAML-mode click would full-replace the
// cluster state with an empty spec (ReplaceSpec=true +
// empty incoming = delete everything).
func TestMarshalSpecForEditHappyPath(t *testing.T) {
	t.Parallel()

	pgh := &PageHandler{log: slog.New(slog.DiscardHandler)}

	got := pgh.marshalSpecForEdit("tenant-x", "app-y", map[string]any{
		"replicas": 3,
		"backup": map[string]any{
			"enabled": true,
		},
	})

	if got == "" {
		t.Fatal("marshalSpecForEdit returned empty on non-empty spec; edit YAML tab would open blank")
	}

	if !strings.Contains(got, "replicas: 3") {
		t.Errorf("marshalled YAML missing replicas: got %q", got)
	}

	if !strings.Contains(got, "enabled: true") {
		t.Errorf("marshalled YAML missing backup.enabled: got %q", got)
	}
}
