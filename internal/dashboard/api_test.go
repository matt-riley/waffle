package dashboard

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestEventsSSEReplayLastEventIDAndResync(t *testing.T) {
	hub := NewEventHub(2)
	hub.Publish(Event{Type: "one"})
	second := hub.Publish(Event{Type: "two", Resource: "run", ResourceID: "run-1", Data: []byte(`{"state":"ok"}`)})
	config := testAPIConfig(t)
	config.Hub = hub

	ctx, cancel := context.WithCancel(context.Background())
	writer := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		newEventsHandler(config).ServeHTTP(writer, newEventsRequest(ctx, second.Cursor-1))
	}()
	writer.waitForFlush(t)
	cancel()
	<-done
	want := "id: 2\nevent: two\ndata: {\"cursor\":2,\"type\":\"two\",\"resource\":\"run\",\"resource_id\":\"run-1\",\"data\":{\"state\":\"ok\"}}\n\n"
	if got := writer.String(); got != want {
		t.Fatalf("SSE replay = %q, want %q", got, want)
	}

	hub.Publish(Event{Type: "three"})
	writer = newStreamRecorder()
	newEventsHandler(config).ServeHTTP(writer, newEventsRequest(context.Background(), 0))
	if got, want := writer.String(), "event: resync_required\ndata: {}\n\n"; got != want {
		t.Fatalf("resync SSE = %q, want %q", got, want)
	}
}

func TestEventsRejectsMalformedLastEventID(t *testing.T) {
	writer := newStreamRecorder()
	request := newEventsRequest(context.Background(), 0)
	request.Header.Set("Last-Event-ID", " 1")
	newEventsHandler(testAPIConfig(t)).ServeHTTP(writer, request)
	if writer.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", writer.status)
	}
}

func TestEventsHeartbeatAndRequestCancellation(t *testing.T) {
	ticks := make(chan time.Time, 1)
	config := testAPIConfig(t)
	config.Heartbeat = func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }
	ctx, cancel := context.WithCancel(context.Background())
	writer := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		newEventsHandler(config).ServeHTTP(writer, newEventsRequest(ctx, 0))
	}()
	ticks <- time.Now()
	writer.waitForFlush(t)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("events handler did not exit after request cancellation")
	}
	if got, want := writer.String(), ": heartbeat\n\n"; got != want {
		t.Fatalf("heartbeat = %q, want %q", got, want)
	}
}

func newEventsRequest(ctx context.Context, cursor uint64) *http.Request {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8422/api/v1/desk/events", nil)
	if cursor != 0 {
		request.Header.Set("Last-Event-ID", stringCursor(cursor))
	}
	return request
}

func stringCursor(cursor uint64) string {
	return strconv.FormatUint(cursor, 10)
}

type streamRecorder struct {
	mu      sync.Mutex
	header  http.Header
	body    bytes.Buffer
	status  int
	flushes chan struct{}
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{header: make(http.Header), status: http.StatusOK, flushes: make(chan struct{}, 16)}
}

func (w *streamRecorder) Header() http.Header    { return w.header }
func (w *streamRecorder) WriteHeader(status int) { w.status = status }
func (w *streamRecorder) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(data)
}
func (w *streamRecorder) Flush()         { w.flushes <- struct{}{} }
func (w *streamRecorder) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.body.String() }

func (w *streamRecorder) waitForFlush(t *testing.T) {
	t.Helper()
	select {
	case <-w.flushes:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SSE flush")
	}
}
