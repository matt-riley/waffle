// Package telegram implements the Telegram channel adapter over the Bot
// API's long-polling getUpdates. Plain stdlib HTTP — the API is two JSON
// endpoints, not worth a dependency — and the base URL is configurable so
// tests (and proxies) can stand in for api.telegram.org.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/matt-riley/waffle/internal/channel"
)

// DefaultBaseURL is the real Bot API.
const DefaultBaseURL = "https://api.telegram.org"

// maxMessageLen is Telegram's hard limit; longer replies are split.
const maxMessageLen = 4000

// Adapter is a Telegram bot connection.
type Adapter struct {
	token   string
	baseURL string
	client  *http.Client
}

// New builds an adapter. baseURL may be empty for the real API.
func New(token, baseURL string) *Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Adapter{
		token:   token,
		baseURL: baseURL,
		// No client-level timeout: getUpdates long-polls. Per-request
		// deadlines come from contexts.
		client: &http.Client{},
	}
}

// Name implements channel.Adapter.
func (a *Adapter) Name() string { return "telegram" }

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Text string `json:"text"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From struct {
			ID        int64  `json:"id"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"from"`
	} `json:"message"`
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

// Run long-polls getUpdates until ctx is done.
func (a *Adapter) Run(ctx context.Context, inbound chan<- channel.Message) error {
	var offset int64
	for {
		updates, err := a.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Transient network/API trouble: back off and keep polling.
			select {
			case <-time.After(3 * time.Second):
				continue
			case <-ctx.Done():
				return nil
			}
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message == nil || u.Message.Text == "" {
				continue
			}
			name := u.Message.From.FirstName
			if name == "" {
				name = u.Message.From.Username
			}
			msg := channel.Message{
				Channel:    a.Name(),
				ChatID:     strconv.FormatInt(u.Message.Chat.ID, 10),
				SenderID:   strconv.FormatInt(u.Message.From.ID, 10),
				SenderName: name,
				Text:       u.Message.Text,
			}
			select {
			case inbound <- msg:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

func (a *Adapter) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	// The request context caps the long poll a little past the server-side
	// timeout so a wedged connection can't hang the loop.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	raw, err := a.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         50,
		"allowed_updates": []string{"message"},
	})
	if err != nil {
		return nil, err
	}
	var updates []update
	if err := json.Unmarshal(raw, &updates); err != nil {
		return nil, fmt.Errorf("telegram: parse updates: %w", err)
	}
	return updates, nil
}

// Send implements channel.Adapter, splitting long replies.
func (a *Adapter) Send(ctx context.Context, chatID, text string) error {
	if text == "" {
		return nil
	}
	for _, chunk := range split(text, maxMessageLen) {
		if _, err := a.call(ctx, "sendMessage", map[string]any{
			"chat_id": chatID,
			"text":    chunk,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/bot%s/%s", a.baseURL, a.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram: %s: %w", method, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body

	var api apiResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8*1024*1024)).Decode(&api); err != nil {
		return nil, fmt.Errorf("telegram: %s: bad response: %w", method, err)
	}
	if !api.OK {
		return nil, fmt.Errorf("telegram: %s: %s", method, api.Description)
	}
	return api.Result, nil
}

// split breaks text into chunks of at most limit bytes, preferring line
// boundaries so code blocks and lists survive. When no line boundary is
// available it cuts on a UTF-8 rune boundary, so a multi-byte character
// (accented text, emoji) is never split into invalid UTF-8.
func split(text string, limit int) []string {
	var chunks []string
	for len(text) > limit {
		cut := runeBoundary(text, limit)
		if i := lastIndexByte(text[:limit], '\n'); i > limit/2 {
			cut = i + 1
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}

// runeBoundary returns the largest index <= limit at which text can be cut
// without splitting a multi-byte rune (i.e. text[index] begins a rune).
func runeBoundary(text string, limit int) int {
	i := limit
	for i > 0 && !utf8.RuneStart(text[i]) {
		i--
	}
	if i == 0 {
		return limit // a single rune longer than limit; cut anyway to progress
	}
	return i
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

var _ channel.Adapter = (*Adapter)(nil)
