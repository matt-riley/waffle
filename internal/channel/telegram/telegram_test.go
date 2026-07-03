package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/matt-riley/waffle/internal/channel"
)

func TestSplitKeepsValidUTF8(t *testing.T) {
	// A run of 3-byte runes with no newline, forced to split mid-run.
	text := strings.Repeat("界", 100) // 300 bytes
	chunks := split(text, 50)

	if strings.Join(chunks, "") != text {
		t.Fatal("chunks don't reassemble to the original")
	}
	for i, c := range chunks {
		if len(c) > 50 {
			t.Errorf("chunk %d is %d bytes, over limit", i, len(c))
		}
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d is not valid UTF-8: %q", i, c)
		}
	}
}

func TestSplitPrefersNewlines(t *testing.T) {
	text := strings.Repeat("line\n", 20) // 100 bytes, newline every 5
	chunks := split(text, 40)
	if strings.Join(chunks, "") != text {
		t.Fatal("chunks don't reassemble")
	}
	// Every chunk but the last should end on a newline boundary.
	for i, c := range chunks[:len(chunks)-1] {
		if !strings.HasSuffix(c, "\n") {
			t.Errorf("chunk %d did not end on a newline: %q", i, c)
		}
	}
}

func TestRunDeliversMessagesAndAdvancesOffset(t *testing.T) {
	var mu sync.Mutex
	var offsets []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/bottest-token/getUpdates") {
			t.Errorf("path = %s", r.URL.Path)
		}
		var req struct {
			Offset int64 `json:"offset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		mu.Lock()
		offsets = append(offsets, req.Offset)
		n := len(offsets)
		mu.Unlock()
		if n == 1 {
			fmt.Fprint(w, `{"ok":true,"result":[
				{"update_id":7,"message":{"text":"hello","chat":{"id":100},"from":{"id":42,"first_name":"Matt"}}},
				{"update_id":8,"message":{"text":"","chat":{"id":100},"from":{"id":42}}},
				{"update_id":9}
			]}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":[]}`)
	}))
	defer srv.Close()

	a := New("test-token", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	inbound := make(chan channel.Message, 4)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, inbound) }()

	select {
	case msg := <-inbound:
		want := channel.Message{Channel: "telegram", ChatID: "100", SenderID: "42", SenderName: "Matt", Text: "hello"}
		if msg != want {
			t.Errorf("msg = %+v, want %+v", msg, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no inbound message")
	}
	// Only the text message is delivered; empty/message-less updates skip.
	select {
	case extra := <-inbound:
		t.Errorf("unexpected extra message: %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(offsets) < 2 || offsets[1] != 10 {
		t.Errorf("offsets = %v, want second poll at 10", offsets)
	}
}

func TestSendSplitsLongMessages(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Errorf("path = %s", r.URL.Path)
		}
		var req struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.ChatID != "100" {
			t.Errorf("chat_id = %q", req.ChatID)
		}
		mu.Lock()
		sent = append(sent, req.Text)
		mu.Unlock()
		fmt.Fprint(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()

	a := New("t", srv.URL)
	long := strings.Repeat("line of text\n", 700) // ~9100 bytes
	if err := a.Send(context.Background(), "100", long); err != nil {
		t.Fatalf("Send: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 3 {
		t.Fatalf("chunks = %d, want 3", len(sent))
	}
	if strings.Join(sent, "") != long {
		t.Error("chunks don't reassemble to the original")
	}
	for i, c := range sent {
		if len(c) > maxMessageLen {
			t.Errorf("chunk %d is %d bytes", i, len(c))
		}
	}
}

func TestAPIErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":false,"description":"Unauthorized"}`)
	}))
	defer srv.Close()

	a := New("bad", srv.URL)
	if err := a.Send(context.Background(), "1", "hi"); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("err = %v", err)
	}
}
