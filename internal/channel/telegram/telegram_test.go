package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
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
			Text: "hello", ChatType: "private", AckID: "7",
		}
		// channel.Message now carries an Attachments slice, so struct
		// equality is a compile error; DeepEqual has identical semantics
		// here (both sides have nil attachments).
		if !reflect.DeepEqual(msg, want) {
			t.Errorf("msg = %+v, want %+v", msg, want)
		}
		a.Ack(msg.AckID)
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

// memoryOffsets is an OffsetStore that records every save.
type memoryOffsets struct {
	mu      sync.Mutex
	loaded  int64
	loadErr error
	saved   []int64
}

func (m *memoryOffsets) Load(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loaded, m.loadErr
}

func (m *memoryOffsets) Save(_ context.Context, offset int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saved = append(m.saved, offset)
	return nil
}

func (m *memoryOffsets) savedOffsets() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int64(nil), m.saved...)
}

// updatesServer serves getMe plus one batch of updates, recording the offset
// of every getUpdates call.
func updatesServer(t *testing.T, batch string) (*httptest.Server, func() []int64) {
	t.Helper()
	var mu sync.Mutex
	var offsets []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"username":"waffle_bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			var req struct {
				Offset int64 `json:"offset"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			mu.Lock()
			offsets = append(offsets, req.Offset)
			first := len(offsets) == 1
			mu.Unlock()
			if first {
				fmt.Fprint(w, batch)
				return
			}
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
		default:
			t.Errorf("path = %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []int64 {
		mu.Lock()
		defer mu.Unlock()
		return append([]int64(nil), offsets...)
	}
}

const oneUpdateBatch = `{"ok":true,"result":[
	{"update_id":7,"message":{"text":"hello","chat":{"id":100,"type":"private"},"from":{"id":42,"first_name":"Matt"}}}
]}`

func TestRunHoldsOffsetUntilDeliveredMessageIsAcked(t *testing.T) {
	srv, polls := updatesServer(t, oneUpdateBatch)
	offsets := &memoryOffsets{}
	a := New("test-token", srv.URL)
	a.Offsets = offsets

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inbound := make(chan channel.Message, 4)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, inbound) }()

	var msg channel.Message
	select {
	case msg = <-inbound:
	case <-time.After(5 * time.Second):
		t.Fatal("no inbound message")
	}

	// Confirming the batch is what tells Telegram to forget it, so an
	// unhandled message must keep the cursor where it is (#257).
	time.Sleep(200 * time.Millisecond)
	if got := polls(); len(got) != 1 {
		t.Fatalf("polls = %v, want the adapter to wait for the ack before polling again", got)
	}
	if saved := offsets.savedOffsets(); len(saved) != 0 {
		t.Fatalf("saved = %v, want nothing saved before the message is handled", saved)
	}

	a.Ack(msg.AckID)

	deadline := time.After(5 * time.Second)
	for {
		if saved := offsets.savedOffsets(); len(saved) > 0 {
			if saved[len(saved)-1] != 8 {
				t.Fatalf("saved = %v, want the cursor past update 7", saved)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("cursor never advanced after the ack")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
	if got := polls(); len(got) < 2 || got[1] != 8 {
		t.Errorf("polls = %v, want the second poll to confirm at 8", got)
	}
}

func TestRunResumesFromStoredOffset(t *testing.T) {
	srv, polls := updatesServer(t, `{"ok":true,"result":[]}`)
	a := New("test-token", srv.URL)
	a.Offsets = &memoryOffsets{loaded: 41}

	ctx, cancel := context.WithCancel(context.Background())
	inbound := make(chan channel.Message, 1)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, inbound) }()

	deadline := time.After(5 * time.Second)
	for len(polls()) == 0 {
		select {
		case <-deadline:
			t.Fatal("no getUpdates call")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
	if got := polls(); got[0] != 41 {
		t.Errorf("first poll offset = %d, want the stored cursor 41", got[0])
	}
}

func TestRunFallsBackToReplayWhenOffsetLoadFails(t *testing.T) {
	srv, polls := updatesServer(t, `{"ok":true,"result":[]}`)
	a := New("test-token", srv.URL)
	a.Offsets = &memoryOffsets{loaded: 41, loadErr: errors.New("database is locked")}

	ctx, cancel := context.WithCancel(context.Background())
	inbound := make(chan channel.Message, 1)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, inbound) }()

	deadline := time.After(5 * time.Second)
	for len(polls()) == 0 {
		select {
		case <-deadline:
			t.Fatal("no getUpdates call")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
	// Replaying what Telegram still holds is recoverable; skipping past it
	// is not, so an unreadable cursor must not be treated as a valid one.
	if got := polls(); got[0] != 0 {
		t.Errorf("first poll offset = %d, want 0 after a failed load", got[0])
	}
}

func TestAckIsIdempotentAndUnblocksOnShutdown(t *testing.T) {
	srv, _ := updatesServer(t, oneUpdateBatch)
	a := New("test-token", srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	inbound := make(chan channel.Message, 4)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, inbound) }()

	var msg channel.Message
	select {
	case msg = <-inbound:
	case <-time.After(5 * time.Second):
		t.Fatal("no inbound message")
	}
	// A duplicate ack must not corrupt the batch's outstanding count.
	a.Ack(msg.AckID)
	a.Ack(msg.AckID)
	a.Ack("nonexistent")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// logCapture collects slog output so tests can assert that no update kind is
// ever dropped silently (#251).
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", r.Level, r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
		return true
	})
	c.lines = append(c.lines, b.String())
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func (c *logCapture) has(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// captureLogs swaps the default logger for a capture and restores it.
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	prev := slog.Default()
	c := &logCapture{}
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

// decodeFixture runs one getUpdates batch through an adapter with the given
// attachment cap, against a fake server, and returns the first delivered
// inbound message.
func decodeFixture(t *testing.T, maxBytes int64, batch string) (channel.Message, bool) {
	t.Helper()
	srv := groupTestServer(t, batch)
	defer srv.Close()
	a := New("t", srv.URL)
	a.MaxAttachmentBytes = maxBytes
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inbound := make(chan channel.Message, 4)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, inbound) }()
	select {
	case msg := <-inbound:
		cancel()
		<-done
		return msg, true
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		return channel.Message{}, false
	}
}

func TestBehaviorDecodesMediaAttachments(t *testing.T) {
	tests := []struct {
		name      string
		batch     string
		mediaType string
		filename  string
		mime      string
		size      int64
		text      string // expected caption/text
		hasText   bool
	}{
		{
			name: "photo with caption", batch: photoUpdate,
			mediaType: "photo", filename: "photo-ph1.jpg", mime: "image/jpeg", size: 81920,
			text: "the washing machine display", hasText: true,
		},
		{
			name: "document with caption", batch: documentUpdate,
			mediaType: "document", filename: "washing-machine-manual.pdf", mime: "application/pdf", size: 2048,
			text: "the manual", hasText: true,
		},
		{
			name: "voice note without caption", batch: voiceUpdate,
			mediaType: "voice", filename: "voice-vc1.ogg", mime: "audio/ogg", size: 4096,
		},
		{
			name: "video without caption", batch: videoUpdate,
			mediaType: "video", filename: "video-vd1.mp4", mime: "video/mp4", size: 16384,
		},
		{
			name: "audio file", batch: audioUpdate,
			mediaType: "audio", filename: "beep.mp3", mime: "audio/mpeg", size: 32768,
		},
		{
			name: "video note", batch: videoNoteUpdate,
			mediaType: "video_note", filename: "video_note-vn1.mp4", mime: "video/mp4", size: 65536,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := decodeFixture(t, 1<<30, tc.batch)
			if !ok {
				t.Fatal("no inbound message delivered")
			}
			if len(msg.Attachments) != 1 {
				t.Fatalf("attachments = %d, want 1 (%+v)", len(msg.Attachments), msg.Attachments)
			}
			att := msg.Attachments[0]
			if att.MediaType != tc.mediaType {
				t.Errorf("media type = %q, want %q", att.MediaType, tc.mediaType)
			}
			if att.Filename != tc.filename {
				t.Errorf("filename = %q, want %q", att.Filename, tc.filename)
			}
			if att.MIME != tc.mime {
				t.Errorf("mime = %q, want %q", att.MIME, tc.mime)
			}
			if att.Size != tc.size {
				t.Errorf("size = %d, want %d", att.Size, tc.size)
			}
			if att.Fetch == "" {
				t.Error("fetch handle missing")
			}
			if att.Skip != "" {
				t.Errorf("skip = %q, want empty", att.Skip)
			}
			if msg.ChatID != "100" || msg.SenderID != "42" || msg.SenderName != "Matt" || msg.ChatType != "private" || msg.IsGroup {
				t.Errorf("message scope = %+v", msg)
			}
			if tc.hasText && msg.Text != tc.text {
				t.Errorf("text = %q, want caption %q", msg.Text, tc.text)
			}
		})
	}
}

// TestBehaviorCaptionAndAttachmentBothSurvive pins #251: a caption is never
// dropped because a photo arrived, and the attachment is never dropped
// because a caption did.
func TestBehaviorCaptionAndAttachmentBothSurvive(t *testing.T) {
	msg, ok := decodeFixture(t, 1<<30, photoUpdate)
	if !ok {
		t.Fatal("no inbound message delivered")
	}
	if msg.Text != "the washing machine display" {
		t.Errorf("text = %q, want the caption preserved", msg.Text)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].MediaType != "photo" {
		t.Errorf("attachments = %+v, want the photo preserved", msg.Attachments)
	}
}

// TestBehaviorEditedMessageIgnoredWithLog pins #251: edits are deliberately
// ignored (the original was already handled) and the ignore is logged, never
// silent.
func TestBehaviorEditedMessageIgnoredWithLog(t *testing.T) {
	logs := captureLogs(t)
	msg, ok := decodeFixture(t, 1<<30, editedMessageUpdate)
	if ok {
		t.Fatalf("edited message delivered: %+v", msg)
	}
	if !logs.has("ignored edited_message") {
		t.Errorf("logs = %v, want an edited_message ignore line", logs.lines)
	}
}

// TestBehaviorUnhandledUpdateKindsAreLogged pins #251: an update with no
// message, a message with no text and no attachment, and a group message
// without a mention each produce a log line instead of vanishing silently.
func TestBehaviorUnhandledUpdateKindsAreLogged(t *testing.T) {
	logs := captureLogs(t)
	batch := `{"ok":true,"result":[
		{"update_id":1},
		{"update_id":2,"message":{"text":"","chat":{"id":100,"type":"private"},"from":{"id":42}}},
		{"update_id":3,"message":{"sticker":{"file_id":"s1"},"chat":{"id":100,"type":"private"},"from":{"id":42}}},
		{"update_id":4,"message":{"text":"just chatting","chat":{"id":-100,"type":"supergroup"},"from":{"id":42}}},
		{"update_id":5,"edited_message":{"text":"edited","chat":{"id":100,"type":"private"},"from":{"id":42}}}
	]}`
	msg, ok := decodeFixture(t, 1<<30, batch)
	if ok {
		t.Fatalf("unexpected delivery: %+v", msg)
	}
	for _, want := range []string{
		"dropped update without a message",
		"dropped message without text or attachment",
		"ignored group message without mention",
		"ignored edited_message",
	} {
		if !logs.has(want) {
			t.Errorf("logs missing %q: %v", want, logs.lines)
		}
	}
}

// TestBehaviorOversizedAttachmentRefusedBeforeFetch pins #251: the size cap
// is enforced from the update's own file_size, before getFile and before the
// file download — the fetch endpoints must never be touched.
func TestBehaviorOversizedAttachmentRefusedBeforeFetch(t *testing.T) {
	fetched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"username":"waffle_bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			fmt.Fprint(w, oversizedPhotoUpdate)
		default:
			fetched = true
			t.Errorf("fetch endpoint hit for oversized attachment: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := New("t", srv.URL)
	a.MaxAttachmentBytes = 1024 * 1024 // 1 MiB; fixture is 500000000 bytes
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inbound := make(chan channel.Message, 4)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, inbound) }()

	select {
	case msg := <-inbound:
		if len(msg.Attachments) != 1 {
			t.Fatalf("attachments = %+v", msg.Attachments)
		}
		att := msg.Attachments[0]
		if att.MediaType != "photo" || att.Size != 500000000 {
			t.Fatalf("att = %+v", att)
		}
		if att.Fetch != "" {
			t.Fatalf("oversized attachment still carries a fetch handle: %q", att.Fetch)
		}
		if !strings.Contains(att.Skip, "1048576") {
			t.Errorf("skip = %q, want the byte limit named", att.Skip)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no inbound message")
	}
	cancel()
	<-done
	if fetched {
		t.Fatal("oversized attachment was fetched")
	}
}

// TestBehaviorDisabledAttachmentsRefusedBeforeFetch pins the deny-by-default
// posture: with MaxAttachmentBytes unset, metadata still reaches the gateway
// but no fetch handle is offered and no download endpoint is touched.
func TestBehaviorDisabledAttachmentsRefusedBeforeFetch(t *testing.T) {
	fetched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"username":"waffle_bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			fmt.Fprint(w, photoUpdate)
		default:
			fetched = true
			t.Errorf("fetch endpoint hit with downloads disabled: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := New("t", srv.URL) // MaxAttachmentBytes zero: disabled
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inbound := make(chan channel.Message, 4)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, inbound) }()

	select {
	case msg := <-inbound:
		att := msg.Attachments[0]
		if att.Fetch != "" {
			t.Fatalf("disabled attachment still carries a fetch handle: %q", att.Fetch)
		}
		if !strings.Contains(att.Skip, "disabled") {
			t.Errorf("skip = %q, want a disabled explanation", att.Skip)
		}
		if msg.Text != "the washing machine display" {
			t.Errorf("caption lost when downloads disabled: %q", msg.Text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no inbound message")
	}
	cancel()
	<-done
	if fetched {
		t.Fatal("attachment was fetched while downloads are disabled")
	}
}

// TestBehaviorGroupAttachmentWithoutMentionDropped pins #251: an attachment
// must not bypass the group mention gate (#34).
func TestBehaviorGroupAttachmentWithoutMentionDropped(t *testing.T) {
	logs := captureLogs(t)
	msg, ok := decodeFixture(t, 1<<30, groupPhotoNoMentionUpdate)
	if ok {
		t.Fatalf("unaddressed group attachment delivered: %+v", msg)
	}
	if !logs.has("ignored group message without mention") {
		t.Errorf("logs = %v, want group gate line", logs.lines)
	}
}

// TestBehaviorGroupAttachmentMentionGateAppliesToCaption pins #251: a group
// photo whose caption mentions the bot is delivered with the mention
// stripped from the caption, exactly like text mentions.
func TestBehaviorGroupAttachmentMentionGateAppliesToCaption(t *testing.T) {
	msg, ok := decodeFixture(t, 1<<30, groupPhotoMentionUpdate)
	if !ok {
		t.Fatal("mentioned group attachment not delivered")
	}
	if !msg.IsGroup || msg.ChatType != "supergroup" || msg.ChatID != "-100" {
		t.Fatalf("scope = %+v", msg)
	}
	if msg.Text != "look at this" {
		t.Errorf("caption = %q, want mention stripped", msg.Text)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].MediaType != "photo" {
		t.Errorf("attachments = %+v", msg.Attachments)
	}
}

// fetchServer serves getMe, getUpdates, getFile, and the file download
// endpoint, recording which were touched.
func fetchServer(t *testing.T, batch, filePath, fileContent string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var touched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		touched = append(touched, r.URL.Path)
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"username":"waffle_bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			fmt.Fprint(w, batch)
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			fmt.Fprintf(w, `{"ok":true,"result":{"file_id":"f1","file_size":%d,"file_path":%q}}`, len(fileContent), filePath)
		case strings.HasSuffix(r.URL.Path, "/file/bott/"+filePath):
			fmt.Fprint(w, fileContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), touched...)
	}
}

func TestBehaviorFetchAttachmentDownloadsBytes(t *testing.T) {
	content := "photo bytes, not real"
	srv, touched := fetchServer(t, photoUpdate, "photos/photo.bin", content)
	a := New("t", srv.URL)
	a.MaxAttachmentBytes = 1 << 20
	data, err := a.FetchAttachment(context.Background(), "photo_id")
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}
	if string(data) != content {
		t.Errorf("data = %q, want %q", data, content)
	}
	paths := touched()
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "/getFile") || !strings.HasSuffix(paths[1], "/file/bott/photos/photo.bin") {
		t.Errorf("touched = %v, want getFile then the file endpoint", paths)
	}
}

func TestBehaviorFetchAttachmentRefusesOverCapBeforeDownload(t *testing.T) {
	srv, touched := fetchServer(t, photoUpdate, "photos/huge.bin", strings.Repeat("x", 2<<20))
	a := New("t", srv.URL)
	a.MaxAttachmentBytes = 1024
	_, err := a.FetchAttachment(context.Background(), "photo_id")
	if err == nil || !strings.Contains(err.Error(), "exceeds the 1024-byte limit") {
		t.Fatalf("err = %v, want cap refusal", err)
	}
	for _, p := range touched() {
		if strings.Contains(p, "/file/bot") {
			t.Fatalf("file endpoint hit despite cap refusal: %v", touched())
		}
	}
}

func TestBehaviorFetchAttachmentGetFileFailureSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":false,"description":"file is too big"}`)
	}))
	defer srv.Close()
	a := New("t", srv.URL)
	a.MaxAttachmentBytes = 1 << 20
	_, err := a.FetchAttachment(context.Background(), "gone")
	if err == nil || !strings.Contains(err.Error(), "file is too big") {
		t.Fatalf("err = %v, want the API error surfaced", err)
	}
}

func TestBehaviorFetchAttachmentCancellationAborts(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			fmt.Fprint(w, `{"ok":true,"result":{"file_id":"f1","file_size":100,"file_path":"files/slow.bin"}}`)
		case strings.HasSuffix(r.URL.Path, "/file/bott/files/slow.bin"):
			<-release // hold the download until the test cancels
			fmt.Fprint(w, "never read")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer close(release)

	a := New("t", srv.URL)
	a.MaxAttachmentBytes = 1 << 20
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.FetchAttachment(ctx, "f1")
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the download start
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FetchAttachment succeeded after cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FetchAttachment did not abort on cancellation")
	}
}

// sendServer captures multipart attachment sends.
func sendServer(t *testing.T) (*httptest.Server, *struct {
	mu       sync.Mutex
	method   string
	chatID   string
	caption  string
	filename string
	data     []byte
}) {
	t.Helper()
	got := &struct {
		mu       sync.Mutex
		method   string
		chatID   string
		caption  string
		filename string
		data     []byte
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		got.mu.Lock()
		defer got.mu.Unlock()
		got.method = r.URL.Path
		got.chatID = r.FormValue("chat_id")
		got.caption = r.FormValue("caption")
		for _, fh := range r.MultipartForm.File {
			f, err := fh[0].Open()
			if err != nil {
				t.Errorf("open part: %v", err)
				continue
			}
			got.filename = fh[0].Filename
			got.data, _ = io.ReadAll(f)
			_ = f.Close()
		}
		fmt.Fprint(w, `{"ok":true,"result":{}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestBehaviorSendAttachmentPhoto(t *testing.T) {
	srv, got := sendServer(t)
	a := New("t", srv.URL)
	att := channel.Attachment{
		MediaType: "photo", Filename: "display.jpg", MIME: "image/jpeg",
		Size: 5, Data: []byte("12345"),
	}
	if err := a.SendAttachment(context.Background(), "100", att, "the display"); err != nil {
		t.Fatalf("SendAttachment: %v", err)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if !strings.HasSuffix(got.method, "/sendPhoto") {
		t.Errorf("method = %s, want sendPhoto", got.method)
	}
	if got.chatID != "100" || got.caption != "the display" {
		t.Errorf("chat_id/caption = %q/%q", got.chatID, got.caption)
	}
	if got.filename != "display.jpg" || string(got.data) != "12345" {
		t.Errorf("file = %q %q", got.filename, got.data)
	}
}

func TestBehaviorSendAttachmentDocument(t *testing.T) {
	srv, got := sendServer(t)
	a := New("t", srv.URL)
	att := channel.Attachment{
		MediaType: "document", Filename: "report.pdf", MIME: "application/pdf",
		Size: 3, Data: []byte("%PDF"),
	}
	if err := a.SendAttachment(context.Background(), "100", att, ""); err != nil {
		t.Fatalf("SendAttachment: %v", err)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if !strings.HasSuffix(got.method, "/sendDocument") {
		t.Errorf("method = %s, want sendDocument", got.method)
	}
	if got.caption != "" {
		t.Errorf("caption = %q, want empty", got.caption)
	}
	if got.filename != "report.pdf" || string(got.data) != "%PDF" {
		t.Errorf("file = %q %q", got.filename, got.data)
	}
}

func TestBehaviorSendAttachmentUnsupportedMediaTypeErrors(t *testing.T) {
	srv, _ := sendServer(t)
	a := New("t", srv.URL)
	att := channel.Attachment{MediaType: "voice", Size: 1, Data: []byte("x")}
	err := a.SendAttachment(context.Background(), "100", att, "")
	if err == nil || !strings.Contains(err.Error(), `media type "voice"`) {
		t.Fatalf("err = %v, want unsupported media type error", err)
	}
}

func TestBehaviorSendAttachmentWithoutBytesErrors(t *testing.T) {
	srv, _ := sendServer(t)
	a := New("t", srv.URL)
	err := a.SendAttachment(context.Background(), "100", channel.Attachment{MediaType: "photo"}, "")
	if err == nil || !strings.Contains(err.Error(), "no bytes") {
		t.Fatalf("err = %v, want no-bytes error", err)
	}
}
