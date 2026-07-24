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
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"username":"waffle_bot"}}`)
			return
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
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
					{"update_id":7,"message":{"text":"hello","chat":{"id":100,"type":"private"},"from":{"id":42,"first_name":"Matt"}}},
					{"update_id":8,"message":{"text":"","chat":{"id":100,"type":"private"},"from":{"id":42}}},
					{"update_id":9}
				]}`)
				return
			}
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
			return
		default:
			t.Errorf("path = %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := New("test-token", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	inbound := make(chan channel.Message, 4)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, inbound) }()

	select {
	case msg := <-inbound:
		want := channel.Message{
			Channel: "telegram", ChatID: "100", SenderID: "42", SenderName: "Matt",
			Text: "hello", ChatType: "private",
		}
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

func TestEnsureBotMarksHealthyBeforeFirstLongPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"username":"waffle_bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			t.Error("ensureBot unexpectedly entered the long poll")
			http.Error(w, "unexpected poll", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	healthy := make(chan struct{}, 1)
	adapter := New("test-token", srv.URL)
	adapter.SetPollObserver(func() {
		select {
		case healthy <- struct{}{}:
		default:
		}
	})
	if err := adapter.ensureBot(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-healthy:
	default:
		t.Fatal("Telegram did not report health after successful getMe")
	}
}

// groupTestServer serves getMe plus a single getUpdates payload, then empty polls.
func groupTestServer(t *testing.T, firstUpdates string) *httptest.Server {
	t.Helper()
	var n int
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"username":"waffle_bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			n++
			if n == 1 {
				fmt.Fprint(w, firstUpdates)
				return
			}
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func TestPrivateChatAlwaysDelivered(t *testing.T) {
	srv := groupTestServer(t, `{"ok":true,"result":[
		{"update_id":1,"message":{"text":"dm hello","chat":{"id":100,"type":"private"},"from":{"id":42,"first_name":"Matt"}}}
	]}`)
	defer srv.Close()

	a := New("t", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inbound := make(chan channel.Message, 2)
	go func() { _ = a.Run(ctx, inbound) }()

	select {
	case msg := <-inbound:
		if msg.IsGroup || msg.Text != "dm hello" || msg.ChatType != "private" {
			t.Fatalf("msg = %+v", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
}

func TestGroupWithoutMentionDropped(t *testing.T) {
	srv := groupTestServer(t, `{"ok":true,"result":[
		{"update_id":1,"message":{"text":"just chatting","chat":{"id":-100,"type":"supergroup"},"from":{"id":42,"first_name":"Matt"}}}
	]}`)
	defer srv.Close()

	a := New("t", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inbound := make(chan channel.Message, 2)
	go func() { _ = a.Run(ctx, inbound) }()

	select {
	case msg := <-inbound:
		t.Fatalf("group message without mention delivered: %+v", msg)
	case <-time.After(400 * time.Millisecond):
	}
}

func TestGroupMentionDeliveredAndStripped(t *testing.T) {
	// "hey @waffle_bot please help" — mention entity spans the @username.
	// Offsets are UTF-16; ASCII so byte offsets match.
	text := "hey @waffle_bot please help"
	srv := groupTestServer(t, fmt.Sprintf(`{"ok":true,"result":[
		{"update_id":1,"message":{
			"text":%q,
			"chat":{"id":-100,"type":"supergroup"},
			"from":{"id":42,"first_name":"Matt"},
			"entities":[{"type":"mention","offset":4,"length":11}]
		}}
	]}`, text))
	defer srv.Close()

	a := New("t", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inbound := make(chan channel.Message, 2)
	go func() { _ = a.Run(ctx, inbound) }()

	select {
	case msg := <-inbound:
		if !msg.IsGroup || msg.ChatType != "supergroup" {
			t.Fatalf("expected group message, got %+v", msg)
		}
		if strings.Join(strings.Fields(msg.Text), " ") != "hey please help" {
			t.Fatalf("text = %q, want mention stripped", msg.Text)
		}
		if a.BotUsername() != "waffle_bot" {
			t.Errorf("cached username = %q", a.BotUsername())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
}

func TestGroupReplyToBotDelivered(t *testing.T) {
	srv := groupTestServer(t, `{"ok":true,"result":[
		{"update_id":1,"message":{
			"text":"follow up",
			"chat":{"id":-100,"type":"group"},
			"from":{"id":42,"first_name":"Matt"},
			"reply_to_message":{"text":"earlier","from":{"id":99,"is_bot":true,"username":"waffle_bot"},"chat":{"id":-100,"type":"group"}}
		}}
	]}`)
	defer srv.Close()

	a := New("t", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inbound := make(chan channel.Message, 2)
	go func() { _ = a.Run(ctx, inbound) }()

	select {
	case msg := <-inbound:
		if !msg.IsGroup || msg.Text != "follow up" {
			t.Fatalf("msg = %+v", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
}

func TestGroupBotCommandAtUsernameDelivered(t *testing.T) {
	text := "/status@waffle_bot"
	srv := groupTestServer(t, fmt.Sprintf(`{"ok":true,"result":[
		{"update_id":1,"message":{
			"text":%q,
			"chat":{"id":-100,"type":"supergroup"},
			"from":{"id":42,"first_name":"Matt"},
			"entities":[{"type":"bot_command","offset":0,"length":%d}]
		}}
	]}`, text, len(text)))
	defer srv.Close()

	a := New("t", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inbound := make(chan channel.Message, 2)
	go func() { _ = a.Run(ctx, inbound) }()

	select {
	case msg := <-inbound:
		if !msg.IsGroup {
			t.Fatalf("msg = %+v", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
}

func TestStripBotMention(t *testing.T) {
	re := botMentionRegexp("waffle_bot")
	got := stripBotMention("hi @Waffle_Bot there", re)
	if strings.Join(strings.Fields(got), " ") != "hi there" {
		t.Fatalf("strip = %q", got)
	}
	if got := stripBotMention("no mention", re); got != "no mention" {
		t.Fatalf("unchanged = %q", got)
	}
}

func TestAddressedToBotHelpers(t *testing.T) {
	re := botMentionRegexp("waffle_bot")
	m := &tgMessage{
		Text: "ping @waffle_bot",
		Entities: []messageEntity{
			{Type: "mention", Offset: 5, Length: 11},
		},
	}
	if !addressedToBot(m, 99, "waffle_bot", re) {
		t.Error("expected mention to address bot")
	}
	if addressedToBot(&tgMessage{Text: "nope"}, 99, "waffle_bot", re) {
		t.Error("plain text should not address bot")
	}
	if !addressedToBot(&tgMessage{Text: "ask @Waffle_Bot instead"}, 99, "waffle_bot", re) {
		t.Error("plain-text mention should address bot")
	}
	if addressedToBot(&tgMessage{Text: "ask @waffle_bot_backup instead"}, 99, "waffle_bot", re) {
		t.Error("username prefix should not address bot")
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
