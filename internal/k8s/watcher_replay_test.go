package k8s

import (
	"testing"
)

// TestReplayBufferSinceReturnsNewerOnly is the core contract: after
// a client disconnects at event N, the server must replay every
// event with ID > N in the order they were recorded. Without this
// an SSE reconnect misses state and the browser drifts from cluster
// reality until the user hits refresh.
func TestReplayBufferSinceReturnsNewerOnly(t *testing.T) {
	t.Parallel()

	buf := &replayBuffer{}
	for i := int64(1); i <= 5; i++ {
		buf.add(&WatchEvent{EventID: i, Type: WatchEventAdded, App: Application{Name: "app-a"}})
	}

	// Client disconnected after id=2, so the replay must include
	// exactly {3, 4, 5} in order.
	missed := buf.since(2)

	if got := len(missed); got != 3 {
		t.Fatalf("since(2) returned %d events, want 3", got)
	}

	for i, evt := range missed {
		want := int64(i + 3)
		if evt.EventID != want {
			t.Errorf("missed[%d].EventID = %d, want %d", i, evt.EventID, want)
		}
	}
}

// TestReplayBufferSinceZeroReturnsEverything covers the "fresh
// connection replaying all buffered state" path. The client
// passes sinceID=0 when it has never seen any events, and we
// should emit every event still in the ring.
func TestReplayBufferSinceZeroReturnsEverything(t *testing.T) {
	t.Parallel()

	buf := &replayBuffer{}
	for i := int64(1); i <= 3; i++ {
		buf.add(&WatchEvent{EventID: i, Type: WatchEventAdded, App: Application{Name: "app-a"}})
	}

	// sinceID=0 means "give me everything you still have."
	missed := buf.since(0)

	if got := len(missed); got != 3 {
		t.Errorf("since(0) returned %d events, want 3", got)
	}
}

// TestReplayBufferWrapsAroundOldestDropped verifies the ring
// buffer actually overwrites the oldest slot once capacity is
// exceeded. Without this guarantee the buffer would leak memory
// in proportion to watch activity.
func TestReplayBufferWrapsAroundOldestDropped(t *testing.T) {
	t.Parallel()

	buf := &replayBuffer{}

	// Push replayBufferSize + 10 events so the oldest 10 fall off.
	total := int64(replayBufferSize + 10)
	for i := int64(1); i <= total; i++ {
		buf.add(&WatchEvent{EventID: i, Type: WatchEventModified, App: Application{Name: "app-a"}})
	}

	// Asking for events since the oldest retained ID should
	// return exactly replayBufferSize entries, and the first
	// one's ID should be 11 (ids 1..10 were dropped).
	missed := buf.since(0)

	if got := len(missed); got != replayBufferSize {
		t.Fatalf("since(0) after overflow returned %d events, want %d", got, replayBufferSize)
	}

	const expectedFirstRetainedID = 11
	if missed[0].EventID != expectedFirstRetainedID {
		t.Errorf("oldest retained event id = %d, want %d", missed[0].EventID, expectedFirstRetainedID)
	}

	const expectedLastRetainedID = int64(replayBufferSize) + 10
	if missed[len(missed)-1].EventID != expectedLastRetainedID {
		t.Errorf("newest retained event id = %d, want %d", missed[len(missed)-1].EventID, expectedLastRetainedID)
	}
}

// TestReplayBufferEmptyReturnsNil covers the early-life case
// where the buffer has never received an event. Calling since
// on it should return nil, not panic.
func TestReplayBufferEmptyReturnsNil(t *testing.T) {
	t.Parallel()

	buf := &replayBuffer{}
	if got := buf.since(0); got != nil {
		t.Errorf("empty buffer since(0) = %v, want nil", got)
	}
}

// TestReplayBufferSincePastLatestReturnsEmpty covers the "up to
// date" case: client's Last-Event-ID is already ahead of every
// buffered event, so replay is a no-op.
func TestReplayBufferSincePastLatestReturnsEmpty(t *testing.T) {
	t.Parallel()

	buf := &replayBuffer{}
	for i := int64(1); i <= 3; i++ {
		buf.add(&WatchEvent{EventID: i, Type: WatchEventAdded, App: Application{Name: "app-a"}})
	}

	if got := buf.since(100); len(got) != 0 {
		t.Errorf("since(100) returned %d events, want 0", len(got))
	}
}
