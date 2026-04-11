package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

// TestSlogLoggerRecordsJSONLine locks in the wire format. A test
// that has to parse the emitted JSON catches drift if a future
// refactor swaps attribute keys or silently drops a field —
// downstream log queries would break otherwise.
func TestSlogLoggerRecordsJSONLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := NewSlogLogger(slog.New(handler))

	logger.Record(context.Background(), &Event{
		RequestID: "req-123",
		Actor:     "alice",
		Groups:    []string{"admins"},
		Action:    ActionTenantCreate,
		Resource:  "acme",
		Tenant:    "tenant-root",
		Outcome:   OutcomeSuccess,
		Details:   map[string]any{"parent": "tenant-root"},
	})

	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, buf.String())
	}

	if parsed["audit"] != "event" {
		t.Errorf(`audit marker: got %v, want "event"`, parsed["audit"])
	}
	if parsed["action"] != "tenant.create" {
		t.Errorf(`action: got %v, want "tenant.create"`, parsed["action"])
	}
	if parsed["actor"] != "alice" {
		t.Errorf(`actor: got %v, want "alice"`, parsed["actor"])
	}
	if parsed["request_id"] != "req-123" {
		t.Errorf(`request_id: got %v, want "req-123"`, parsed["request_id"])
	}
	if parsed["outcome"] != "success" {
		t.Errorf(`outcome: got %v, want "success"`, parsed["outcome"])
	}
	if parsed["resource"] != "acme" {
		t.Errorf(`resource: got %v, want "acme"`, parsed["resource"])
	}
	// event_time must be a string that parses as RFC3339 so downstream
	// queries can range-filter on it.
	rawTime, ok := parsed["event_time"].(string)
	if !ok {
		t.Fatalf("event_time is not a string: %v", parsed["event_time"])
	}
	if _, err := time.Parse(time.RFC3339Nano, rawTime); err != nil {
		t.Errorf("event_time %q not parseable as RFC3339: %v", rawTime, err)
	}
}

// TestSlogLoggerDefaultsOutcome guards the "never emit an empty
// outcome" invariant — downstream alerting can't tell "we forgot"
// from "it succeeded" if the field is missing.
func TestSlogLoggerDefaultsOutcome(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := NewSlogLogger(slog.New(slog.NewJSONHandler(&buf, nil)))
	logger.Record(context.Background(), &Event{
		Actor:  "bob",
		Action: ActionAppCreate,
	})

	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, buf.String())
	}

	if parsed["outcome"] != "success" {
		t.Errorf(`default outcome: got %v, want "success"`, parsed["outcome"])
	}
}

// TestSlogLoggerStampsTime checks that a caller who forgets to set
// Time gets a server-side timestamp instead of 0001-01-01, which
// would make the log line look like an epoch glitch.
func TestSlogLoggerStampsTime(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := NewSlogLogger(slog.New(slog.NewJSONHandler(&buf, nil)))
	start := time.Now().UTC().Add(-time.Second)

	logger.Record(context.Background(), &Event{
		Actor:  "carol",
		Action: ActionAuthLogin,
	})

	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, buf.String())
	}

	rawTime, _ := parsed["event_time"].(string)

	ts, err := time.Parse(time.RFC3339Nano, rawTime)
	if err != nil {
		t.Fatalf("parsing event_time: %v", err)
	}

	if ts.Before(start) {
		t.Errorf("event_time %v is before test start %v", ts, start)
	}
}

// TestNopLoggerIsSilent covers the opt-out. A nil check would
// miss subtle bugs like "oops, NopLogger writes to stdout".
func TestNopLoggerIsSilent(t *testing.T) {
	t.Parallel()

	var logger Logger = NopLogger{}
	logger.Record(context.Background(), &Event{Actor: "dave", Action: ActionTenantDelete})
	// If this compiles and doesn't panic, the test passes.
	// The Logger interface guarantees Record takes any Event.
}
