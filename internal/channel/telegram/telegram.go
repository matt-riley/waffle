// Package telegram implements the Telegram channel adapter over the Bot
// API's long-polling getUpdates. Plain stdlib HTTP — the API is two JSON
// endpoints, not worth a dependency — and the base URL is configurable so
// tests (and proxies) can stand in for api.telegram.org.
//
// Group-chat posture (#34): multi-party chats (group/supergroup/channel) are
// mention-gated. Only messages that @mention the bot or reply to it are
// delivered inbound; the mention is stripped from the text. The bot's own
// username is resolved once via getMe and cached — never hardcoded.
package telegram

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
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
	onPoll  func()

	// Bot identity from getMe, cached after the first successful call.
	botMu   sync.Mutex
	botID   int64
	botUser string // username without leading @
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

// SetPollObserver installs a callback invoked after each successful poll.
func (a *Adapter) SetPollObserver(fn func()) { a.onPoll = fn }

// BotUsername returns the cached bot username (without @), or empty if
// getMe has not succeeded yet. Intended for tests.
func (a *Adapter) BotUsername() string {
	a.botMu.Lock()
	defer a.botMu.Unlock()
	return a.botUser
}

type messageEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
	User   *struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		IsBot    bool   `json:"is_bot"`
	} `json:"user"`
}

type tgUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
	IsBot     bool   `json:"is_bot"`
}

type tgMessage struct {
	Text string `json:"text"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	From           tgUser          `json:"from"`
	ReplyToMessage *tgMessage      `json:"reply_to_message"`
	Entities       []messageEntity `json:"entities"`
}

type update struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

// Run long-polls getUpdates until ctx is done.
func (a *Adapter) Run(ctx context.Context, inbound chan<- channel.Message) error {
	// Resolve bot identity up front so group mention gating works on the
	// first update. Transient failures are retried on demand later.
	if err := a.ensureBot(ctx); err != nil && ctx.Err() == nil {
		slog.Default().Warn("telegram getMe failed; will retry", "err", err)
	}

	var offset int64
	consecutive := 0
	for {
		updates, err := a.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Exponential backoff with jitter for long-poll robustness.
			// 1s, 2s, 4s, 8s, 16s, 32s cap + up to 1s jitter (crypto random).
			d := time.Duration(1<<min(consecutive, 5)) * time.Second
			if j, err := rand.Int(rand.Reader, big.NewInt(int64(time.Second))); err == nil {
				d += time.Duration(j.Int64())
			}
			if consecutive > 3 {
				slog.Default().Error("telegram getUpdates persistent errors, backing off", "consecutive", consecutive, "backoff", d, "err", err)
			}
			consecutive++
			select {
			case <-time.After(d):
				continue
			case <-ctx.Done():
				return nil
			}
		}
		consecutive = 0
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			msg, ok := a.toInbound(ctx, u.Message)
			if !ok {
				continue
			}
			select {
			case inbound <- msg:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// toInbound converts a Telegram message into a channel.Message. Returns
// ok=false when the update should be dropped (empty text, or group chat
// without an @mention / reply-to-bot).
func (a *Adapter) toInbound(ctx context.Context, m *tgMessage) (channel.Message, bool) {
	if m == nil || m.Text == "" {
		return channel.Message{}, false
	}
	chatType := m.Chat.Type
	isGroup := isGroupChat(chatType)
	text := m.Text

	if isGroup {
		if err := a.ensureBot(ctx); err != nil {
			slog.Default().Error("telegram getMe for group gate", "err", err)
			return channel.Message{}, false
		}
		botID, botUser := a.botIdentity()
		if !addressedToBot(m, botID, botUser) {
			return channel.Message{}, false
		}
		// Strip @bot so the agent sees the owner's intent, not the address.
		// A bare @mention may leave empty text; still deliver so a nudge runs.
		text = stripBotMention(text, botUser)
	}

	name := m.From.FirstName
	if name == "" {
		name = m.From.Username
	}
	return channel.Message{
		Channel:    a.Name(),
		ChatID:     strconv.FormatInt(m.Chat.ID, 10),
		SenderID:   strconv.FormatInt(m.From.ID, 10),
		SenderName: name,
		Text:       text,
		IsGroup:    isGroup,
		ChatType:   chatType,
	}, true
}

func isGroupChat(chatType string) bool {
	switch chatType {
	case "group", "supergroup", "channel":
		return true
	default:
		return false
	}
}

// addressedToBot reports whether the message @mentions the bot or is a
// reply to one of the bot's messages.
func addressedToBot(m *tgMessage, botID int64, botUser string) bool {
	if m.ReplyToMessage != nil && m.ReplyToMessage.From.ID == botID {
		return true
	}
	if botUser == "" && botID == 0 {
		return false
	}
	needle := "@" + strings.ToLower(botUser)
	for _, e := range m.Entities {
		switch e.Type {
		case "mention":
			if strings.EqualFold(entityText(m.Text, e), "@"+botUser) {
				return true
			}
		case "text_mention":
			if e.User != nil && e.User.ID == botID {
				return true
			}
		case "bot_command":
			// /cmd@botusername is how Telegram addresses a bot in groups.
			cmd := entityText(m.Text, e)
			if i := strings.Index(cmd, "@"); i >= 0 && strings.EqualFold(cmd[i+1:], botUser) {
				return true
			}
		}
	}
	// Fallback: plain-text @username (entities missing in some proxies).
	if botUser != "" && strings.Contains(strings.ToLower(m.Text), needle) {
		return true
	}
	return false
}

// entityText extracts the entity span from text. Telegram entity offsets
// are in UTF-16 code units.
func entityText(text string, e messageEntity) string {
	runes := utf16.Encode([]rune(text))
	if e.Offset < 0 || e.Length < 0 || e.Offset+e.Length > len(runes) {
		return ""
	}
	return string(utf16.Decode(runes[e.Offset : e.Offset+e.Length]))
}

// stripBotMention removes @botusername occurrences (case-insensitive) and
// trims surrounding whitespace.
func stripBotMention(text, botUser string) string {
	if botUser == "" {
		return strings.TrimSpace(text)
	}
	re := regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(botUser) + `\b`)
	return strings.TrimSpace(re.ReplaceAllString(text, ""))
}

func (a *Adapter) botIdentity() (id int64, user string) {
	a.botMu.Lock()
	defer a.botMu.Unlock()
	return a.botID, a.botUser
}

// ensureBot loads the bot's id/username via getMe once and caches them.
func (a *Adapter) ensureBot(ctx context.Context) error {
	a.botMu.Lock()
	if a.botUser != "" {
		a.botMu.Unlock()
		return nil
	}
	a.botMu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	raw, err := a.call(callCtx, "getMe", map[string]any{})
	cancel()
	if err != nil {
		return err
	}
	var me struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(raw, &me); err != nil {
		return fmt.Errorf("telegram: parse getMe: %w", err)
	}
	if me.Username == "" {
		return fmt.Errorf("telegram: getMe returned empty username")
	}
	a.botMu.Lock()
	a.botID = me.ID
	a.botUser = me.Username
	a.botMu.Unlock()
	return nil
}

func (a *Adapter) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	// The request context caps the long poll a little past the server-side
	// timeout so a wedged connection can't hang the loop.
	// Small bounded retry for transient before surfacing to caller.
	for attempt := 0; attempt < 2; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		raw, err := a.call(callCtx, "getUpdates", map[string]any{
			"offset":          offset,
			"timeout":         50,
			"allowed_updates": []string{"message"},
		})
		cancel()
		if err == nil {
			if a.onPoll != nil {
				a.onPoll()
			}
			var updates []update
			if err := json.Unmarshal(raw, &updates); err != nil {
				return nil, fmt.Errorf("telegram: parse updates: %w", err)
			}
			return updates, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// brief retry delay inside getUpdates
		select {
		case <-time.After(time.Duration(attempt+1) * 250 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// final attempt
	callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	raw, err := a.call(callCtx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         50,
		"allowed_updates": []string{"message"},
	})
	cancel()
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

func (a *Adapter) call(ctx context.Context, method string, params map[string]any) (result json.RawMessage, err error) {
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
	defer func() {
		if cerr := resp.Body.Close(); err == nil {
			err = cerr
		}
	}()

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
