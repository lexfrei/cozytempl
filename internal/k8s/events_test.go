package k8s

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestToEventAllFields walks the full path through toEvent — type, reason,
// message, involvedObject, source (component + host), count, firstTimestamp,
// lastTimestamp. Makes sure the simplified struct mirrors the upstream
// Event shape without losing anything the UI cares about.
func TestToEventAllFields(t *testing.T) {
	t.Parallel()

	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"type":    "Warning",
			"reason":  "BackoffLimitExceeded",
			"message": "Job has reached the specified backoff limit",
			"involvedObject": map[string]any{
				"kind": "Job",
				"name": "backup-run-42",
			},
			"source": map[string]any{
				"component": "job-controller",
				"host":      "node-1",
			},
			"count":          int64(5),
			"firstTimestamp": "2026-04-10T12:00:00Z",
			"lastTimestamp":  "2026-04-10T12:15:30Z",
		},
	}

	evt := toEvent(obj)

	if evt.Type != "Warning" {
		t.Errorf("Type = %q, want Warning", evt.Type)
	}
	if evt.Reason != "BackoffLimitExceeded" {
		t.Errorf("Reason = %q", evt.Reason)
	}
	if evt.Object != "Job/backup-run-42" {
		t.Errorf("Object = %q, want Job/backup-run-42", evt.Object)
	}
	if evt.Source != "job-controller on node-1" {
		t.Errorf("Source = %q, want 'job-controller on node-1'", evt.Source)
	}
	if evt.Count != 5 {
		t.Errorf("Count = %d, want 5", evt.Count)
	}
	if evt.FirstSeen.IsZero() {
		t.Error("FirstSeen is zero; expected parsed timestamp")
	}
	if evt.LastSeen.IsZero() {
		t.Error("LastSeen is zero; expected parsed timestamp")
	}
}

// TestToEventFallbackToEventTime covers the newer events.k8s.io layout
// where lastTimestamp is absent and eventTime holds the value. pickEventLastSeen
// should transparently use either.
func TestToEventFallbackToEventTime(t *testing.T) {
	t.Parallel()

	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"type":      "Normal",
			"reason":    "Scheduled",
			"eventTime": "2026-04-10T12:00:00.123456789Z",
		},
	}

	evt := toEvent(obj)

	if evt.LastSeen.IsZero() {
		t.Fatal("LastSeen is zero — fallback to eventTime did not fire")
	}
	wantYear := 2026
	if evt.LastSeen.Year() != wantYear {
		t.Errorf("LastSeen year = %d, want %d", evt.LastSeen.Year(), wantYear)
	}
}

// TestToEventSourceVariants covers the four combinations of (component, host)
// presence that produce different Source strings.
func TestToEventSourceVariants(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		component string
		host      string
		want      string
	}{
		{"both", "kubelet", "node-1", "kubelet on node-1"},
		{"componentOnly", "kubelet", "", "kubelet"},
		{"hostOnly", "", "node-1", "node-1"},
		{"neither", "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			obj := &unstructured.Unstructured{
				Object: map[string]any{
					"source": map[string]any{
						"component": tc.component,
						"host":      tc.host,
					},
				},
			}

			if got := eventSource(obj); got != tc.want {
				t.Errorf("eventSource(%+v) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestToSortedEventsLimit confirms the sort-newest-first behaviour and the
// limit cap.
func TestToSortedEventsLimit(t *testing.T) {
	t.Parallel()

	const (
		reasonFirst  = "first"
		reasonSecond = "second"
		reasonThird  = "third"
	)

	makeEvt := func(reason, ts string) unstructured.Unstructured {
		return unstructured.Unstructured{
			Object: map[string]any{
				"reason":        reason,
				"lastTimestamp": ts,
			},
		}
	}

	items := []unstructured.Unstructured{
		makeEvt(reasonFirst, "2026-04-10T10:00:00Z"),
		makeEvt(reasonThird, "2026-04-10T12:00:00Z"),
		makeEvt(reasonSecond, "2026-04-10T11:00:00Z"),
	}

	// No limit: everything, sorted newest first.
	sorted := toSortedEvents(items, 0)
	if len(sorted) != 3 {
		t.Fatalf("unlimited sort returned %d, want 3", len(sorted))
	}
	if sorted[0].Reason != reasonThird || sorted[2].Reason != reasonFirst {
		t.Errorf("sort order: %v, want third > second > first",
			[]string{sorted[0].Reason, sorted[1].Reason, sorted[2].Reason})
	}

	// Limit 2: drop the oldest.
	limited := toSortedEvents(items, 2)
	if len(limited) != 2 {
		t.Fatalf("limited sort returned %d, want 2", len(limited))
	}
	if limited[0].Reason != reasonThird || limited[1].Reason != reasonSecond {
		t.Errorf("limited sort wrong: %v", limited)
	}
}

func TestNameDerivedFromRelease(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		objName string
		release string
		want    bool
	}{
		{"exact match", "myapp", "myapp", true},
		{"statefulset pod suffix", "myapp-0", "myapp", true},
		{"cnpg instance suffix", "myapp-1", "myapp", true},
		{"headless service suffix", "myapp-r", "myapp", true},
		{"pvc prefix", "data-myapp", "myapp", true},
		{"pvc prefix with ordinal", "data-myapp-0", "myapp", true},
		{"rollout hash segment", "foo-myapp-bar", "myapp", true},

		// Substring but NOT a hyphen-delimited segment should miss —
		// release "myapp" must not match "myapp2-foo" or "premyapp".
		{"neighbour app name", "myapp2-foo", "myapp", false},
		{"concatenated prefix", "premyapp-0", "myapp", false},
		{"concatenated suffix", "foo-myappy", "myapp", false},

		// Degenerate inputs: empty either side is a miss, never a match.
		{"empty release", "myapp-0", "", false},
		{"empty object name", "", "myapp", false},
		{"both empty", "", "", false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := nameDerivedFromRelease(testCase.objName, testCase.release)
			if got != testCase.want {
				t.Errorf("nameDerivedFromRelease(%q, %q) = %v, want %v",
					testCase.objName, testCase.release, got, testCase.want)
			}
		})
	}
}

func TestDedupeByObjectReason(t *testing.T) {
	t.Parallel()

	newer := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	older := time.Date(2026, 4, 10, 11, 0, 0, 0, time.UTC)

	events := []Event{
		// Assume input is already newest-first (toSortedEvents gives
		// that). The first occurrence of each (Object, Reason) wins;
		// subsequent entries are dropped.
		{Object: "Pod/myapp-0", Reason: "BackOff", LastSeen: newer, Count: 5},
		{Object: "Pod/myapp-0", Reason: "BackOff", LastSeen: older, Count: 1},
		{Object: "Pod/myapp-0", Reason: "Pulled", LastSeen: older, Count: 1},
		{Object: "Pod/myapp-1", Reason: "BackOff", LastSeen: older, Count: 1},
	}

	got := dedupeByObjectReason(events, 0)

	if len(got) != 3 {
		t.Fatalf("dedupe yielded %d events, want 3", len(got))
	}

	// Keyed on the first occurrence, which was the newest one.
	if got[0].Count != 5 {
		t.Errorf("first entry kept the wrong occurrence (Count=%d, want 5)", got[0].Count)
	}

	// Limit cap still works after dedupe.
	capped := dedupeByObjectReason(events, 2)
	if len(capped) != 2 {
		t.Errorf("limit=2 returned %d, want 2", len(capped))
	}
}

// TestParseEventTimeFallback confirms parseEventTime returns a zero Time
// on empty input or a bad format, instead of propagating the parse error.
func TestParseEventTimeFallback(t *testing.T) {
	t.Parallel()

	if !parseEventTime("", time.RFC3339).IsZero() {
		t.Error("empty string should produce zero Time")
	}
	if !parseEventTime("not a timestamp", time.RFC3339).IsZero() {
		t.Error("garbage should produce zero Time")
	}

	parsed := parseEventTime("2026-04-10T12:00:00Z", time.RFC3339)
	if parsed.IsZero() {
		t.Error("valid RFC3339 should parse")
	}
}
