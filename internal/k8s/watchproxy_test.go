package k8s

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// TestWatchProxyStreamClearsTimeout pins the cycle-2 blocker: the
// user-credentialed rest.Config has a 10 s client-side Timeout
// (BuildUserRESTConfig) that kills watch streams. WatchProxy.Stream
// must zero that out; without the fix this test observes the
// request canceled when the client timeout fires.
//
// Drives a fake apiserver that holds the watch response open for
// longer than the typical 10 s Timeout — if WatchProxy.Stream
// forgets to reset Timeout, the HTTP client cancels the request
// and the stream closes well before the ctx deadline we set.
func TestWatchProxyStreamClearsTimeout(t *testing.T) {
	t.Parallel()

	// streamReady signals that the fake apiserver has accepted the
	// watch request and begun holding the connection open. The test
	// observes this to prove the watch is genuinely attached (not
	// rejected at the handshake).
	streamReady := make(chan struct{})

	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		// Only respond to the watch request; anything else is a
		// bug in the test harness.
		if !strings.Contains(req.URL.Path, "/namespaces/ns/events") {
			http.NotFound(writer, req)

			return
		}

		// Set chunked streaming so client-go sees an open body.
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)

		flusher, _ := writer.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}

		once.Do(func() { close(streamReady) })

		// Hold the connection open until the request context fires.
		// With Timeout=0 on the client the only thing that should
		// cancel us is the test's own ctx, which we give 3 s. With
		// Timeout=10s left at its default the http.Client would
		// cancel earlier — but we time out the test AFTER 1 s, so
		// the critical assertion is simply "did the stream stay
		// open for 1 s of streaming".
		<-req.Context().Done()
	}))
	defer srv.Close()

	// baseCfg points at the fake apiserver. We deliberately build
	// the user config via BuildUserRESTConfig so the 10 s Timeout
	// is applied — the whole point is to prove WatchProxy strips
	// it back out.
	userCfg := &rest.Config{
		Host: srv.URL,
		// Tight 100ms timeout makes the regression loud: without
		// the Stream fix, the watch is killed in well under a
		// second. With the fix, 100ms never surfaces.
		Timeout: 100 * time.Millisecond,
	}

	proxy := NewWatchProxy()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	watcher, err := proxy.Stream(
		ctx, userCfg,
		schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"},
		"ns",
	)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	defer watcher.Stop()

	// Give the fake server a moment to accept and start holding
	// the stream open. 500 ms is generous — long enough that the
	// bug's 100ms client timeout would absolutely have fired.
	select {
	case <-streamReady:
	case <-time.After(1 * time.Second):
		t.Fatal("fake apiserver never saw the watch request; WatchProxy.Stream must have failed upstream")
	}

	// Hold for 500 ms past the client timeout. If Timeout was not
	// cleared, the watch channel would be closed by now.
	time.Sleep(500 * time.Millisecond)

	select {
	case _, ok := <-watcher.ResultChan():
		if !ok {
			t.Fatal("watch channel closed within the client-timeout window; Stream must set Timeout=0")
		}
	default:
		// ResultChan empty but not closed — exactly what we want:
		// the stream is still open, just no data flowing yet.
	}
}
