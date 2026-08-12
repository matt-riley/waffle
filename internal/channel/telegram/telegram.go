// Package telegram implements the Telegram channel adapter over the Bot
// API's long-polling getUpdates. Plain stdlib HTTP — the API is two JSON
// endpoints, not worth a dependency — and the base URL is configurable so
// tests (and proxies) can stand in for api.telegram.org.
//
// Group-chat posture (#34): multi-party chats (group/supergroup/channel) are
// mention-gated. Only messages that @mention the bot or reply to it are
// delivered inbound; the mention is stripped from the text. The bot's own
// username is resolved once via getMe and cached — never hardcoded.
//
// Attachments (#251): media (photo/document/voice/video/audio/video_note)
// is decoded to metadata plus a fetch handle without any download. The
// gateway resolves the handle through FetchAttachment only for admitted
// senders, inside the conversation lock; the size cap (MaxAttachmentBytes,
// deny-by-default) is enforced before the file is fetched. Downloads stay
// in memory — no temp file, so there is no world-readable path and nothing
// to leak on error or cancellation. Outbound, SendAttachment uploads bytes
// via sendPhoto/sendDocument.
package telegram

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/matt-riley/waffle/internal/channel"
	"github.com/matt-riley/waffle/internal/textcut"
)

// DefaultBaseURL is the real Bot API.
const DefaultBaseURL = "https://api.telegram.org"

// maxMessageLen is Telegram's hard limit; longer replies are split.
const maxMessageLen = 4000

// OffsetStore persists the getUpdates cursor across restarts (#257). Nil
// leaves the cursor in process memory, which is only safe for tests and
// one-shot runs.
type OffsetStore interface {
	Load(ctx context.Context) (int64, error)
	Save(ctx context.Context, offset int64) error
}

// Adapter is a Telegram bot connection.
type Adapter struct {
	token   string
	baseURL string
	client  *http.Client
	onPoll  func()

	// MaxAttachmentBytes caps inbound attachment downloads; zero disables
	// them entirely (deny-by-default). Inbound attachments are data from
	// other people, so the cap is enforced before any fetch, both from the
	// update's own file_size and again from getFile's.
	MaxAttachmentBytes int64

	// Offsets, when set, makes the update cursor durable.
	Offsets OffsetStore

	// acks holds the release func for each delivered-but-unconfirmed update,
	// keyed by AckID. Ack removes and calls it; Run waits for the map to
	// drain before confirming the batch to Telegram.
	ackMu sync.Mutex
	acks  map[string]func()

	// Bot identity from getMe, cached after the first successful call.
	botMu        sync.Mutex
	botID        int64
	botUser      string         // username without leading @
	botMentionRe *regexp.Regexp // matches "@botUser" mentions, compiled once with botUser
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

// SetPollObserver installs a health callback invoked after Telegram first
// authenticates the bot and after each successful long poll. The initial
// getMe signal avoids making process readiness wait for an otherwise idle
// 50-second getUpdates request.
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

type tgPhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
}

type tgDocument struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

type tgAudio struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

type tgVoice struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

type tgVideo struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

type tgVideoNote struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size"`
}

type tgMessage struct {
	Text      string        `json:"text"`
	Caption   string        `json:"caption"`
	Photo     []tgPhotoSize `json:"photo"`
	Document  *tgDocument   `json:"document"`
	Voice     *tgVoice      `json:"voice"`
	Video     *tgVideo      `json:"video"`
	Audio     *tgAudio      `json:"audio"`
	VideoNote *tgVideoNote  `json:"video_note"`
	Chat      struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	From            tgUser          `json:"from"`
	ReplyToMessage  *tgMessage      `json:"reply_to_message"`
	Entities        []messageEntity `json:"entities"`
	CaptionEntities []messageEntity `json:"caption_entities"`
}

type update struct {
	UpdateID      int64      `json:"update_id"`
	Message       *tgMessage `json:"message"`
	EditedMessage *tgMessage `json:"edited_message"`
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

// Run long-polls getUpdates until ctx is done.
//
// Delivery is at-least-once (#257). Telegram confirms updates by the offset
// sent on the *next* getUpdates call, and never resends a confirmed update, so
// the offset may only advance past updates that have been handled: Run hands a
// batch to the gateway, waits for every delivered message to be acked, and only
// then advances and persists the cursor. A process that dies mid-handle leaves
// those updates unconfirmed, and Telegram redelivers them (it retains
// unconfirmed updates for ~24h). Duplicates are therefore possible where an
// exactly-once protocol would need a durable inbox; silent loss is not.
func (a *Adapter) Run(ctx context.Context, inbound chan<- channel.Message) error {
	// Resolve bot identity up front so group mention gating works on the
	// first update. Transient failures are retried on demand later.
	if err := a.ensureBot(ctx); err != nil && ctx.Err() == nil {
		slog.Default().Warn("telegram getMe failed; will retry", "err", err)
	}

	offset := a.loadOffset(ctx)
	defer a.releaseAcks()
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
		next := offset
		var handled sync.WaitGroup
		for _, u := range updates {
			if u.UpdateID >= next {
				next = u.UpdateID + 1
			}
			if u.EditedMessage != nil {
				// #251: edits are deliberately ignored — the original was
				// already delivered and handled, and reprocessing would
				// duplicate replies. Deliberate, and never silent.
				slog.Default().Info("telegram: ignored edited_message", "chat", u.EditedMessage.Chat.ID, "update_id", u.UpdateID)
				continue
			}
			msg, ok := a.toInbound(ctx, u.Message)
			if !ok {
				// Nothing to hand over, so nothing to wait for: the drop was
				// already logged by toInbound with its reason.
				continue
			}
			msg.AckID = strconv.FormatInt(u.UpdateID, 10)
			handled.Add(1)
			a.expectAck(msg.AckID, handled.Done)
			select {
			case inbound <- msg:
			case <-ctx.Done():
				return nil
			}
		}
		// The next poll's offset is what confirms this batch, so it may not
		// be sent until the batch has been handled.
		if !waitFor(ctx, &handled) {
			return nil
		}
		if next != offset {
			offset = next
			a.saveOffset(ctx, offset)
		}
	}
}

// expectAck registers a delivered update as awaiting confirmation. release is
// called at most once however many times the gateway acks.
func (a *Adapter) expectAck(ackID string, release func()) {
	a.ackMu.Lock()
	defer a.ackMu.Unlock()
	if a.acks == nil {
		a.acks = make(map[string]func())
	}
	a.acks[ackID] = sync.OnceFunc(release)
}

// Ack implements channel.Acknowledger: the gateway has finished handling the
// update, so it may be confirmed to Telegram.
func (a *Adapter) Ack(ackID string) {
	a.ackMu.Lock()
	release := a.acks[ackID]
	delete(a.acks, ackID)
	a.ackMu.Unlock()
	if release != nil {
		release()
	}
}

// releaseAcks unblocks anything still waiting when Run returns, so a shutdown
// with messages in flight cannot leak the waiter goroutine.
func (a *Adapter) releaseAcks() {
	a.ackMu.Lock()
	pending := a.acks
	a.acks = nil
	a.ackMu.Unlock()
	for _, release := range pending {
		release()
	}
}

// waitFor reports whether every delivered message in the batch was handled.
// False means ctx ended first and the batch must stay unconfirmed.
func waitFor(ctx context.Context, handled *sync.WaitGroup) bool {
	done := make(chan struct{})
	go func() {
		handled.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// loadOffset reads the durable cursor. A failure is loud but not fatal: the
// cursor falls back to zero, which replays whatever Telegram still holds
// unconfirmed rather than skipping past it.
func (a *Adapter) loadOffset(ctx context.Context) int64 {
	if a.Offsets == nil {
		return 0
	}
	offset, err := a.Offsets.Load(ctx)
	if err != nil {
		slog.Default().Error("telegram offset load failed; replaying unconfirmed updates", "err", err)
		return 0
	}
	return offset
}

// saveOffset persists the cursor after a batch is handled. On failure the
// in-memory cursor still advances — re-polling handled updates would deliver
// them twice — but a restart resumes from the last stored cursor, so the
// failure is reported rather than dropped.
func (a *Adapter) saveOffset(ctx context.Context, offset int64) {
	if a.Offsets == nil {
		return
	}
	// Detached: the batch is already handled, and a shutdown cancelling this
	// write is exactly the case that would replay it on the next start.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.Offsets.Save(ctx, offset); err != nil {
		slog.Default().Error("telegram offset save failed; a restart will replay from the last stored offset", "offset", offset, "err", err)
	}
}

// toInbound converts a Telegram message into a channel.Message. Returns
// ok=false when the update should be dropped. No update is dropped without a
// trace: every drop is logged with its reason.
func (a *Adapter) toInbound(ctx context.Context, m *tgMessage) (channel.Message, bool) {
	if m == nil {
		slog.Default().Info("telegram: dropped update without a message")
		return channel.Message{}, false
	}
	// Media-bearing messages carry their text in the caption.
	text := m.Text
	if text == "" && m.Caption != "" {
		text = m.Caption
	}
	attachments := a.attachments(m)
	if text == "" && len(attachments) == 0 {
		slog.Default().Info("telegram: dropped message without text or attachment", "chat", m.Chat.ID)
		return channel.Message{}, false
	}
	chatType := m.Chat.Type
	isGroup := isGroupChat(chatType)

	if isGroup {
		if err := a.ensureBot(ctx); err != nil {
			slog.Default().Error("telegram getMe for group gate", "err", err)
			return channel.Message{}, false
		}
		botID, botUser, mentionRe := a.botIdentity()
		if !addressedToBot(m, botID, botUser, mentionRe) {
			slog.Default().Info("telegram: ignored group message without mention", "chat", m.Chat.ID)
			return channel.Message{}, false
		}
		// Strip @bot so the agent sees the owner's intent, not the address.
		// A bare @mention may leave empty text; still deliver so a nudge runs.
		text = stripBotMention(text, mentionRe)
	}

	name := m.From.FirstName
	if name == "" {
		name = m.From.Username
	}
	return channel.Message{
		Channel:     a.Name(),
		ChatID:      strconv.FormatInt(m.Chat.ID, 10),
		SenderID:    strconv.FormatInt(m.From.ID, 10),
		SenderName:  name,
		Text:        text,
		Attachments: attachments,
		IsGroup:     isGroup,
		ChatType:    chatType,
	}, true
}

// attachments decodes media metadata from a Telegram message. Bytes are
// never fetched here: the gateway resolves the Fetch handle only for
// admitted senders. The configured size cap is enforced here, before any
// fetch — an attachment over it (or with downloads disabled) is refused
// outright and carries a Skip reason instead of a handle.
func (a *Adapter) attachments(m *tgMessage) []channel.Attachment {
	limit := a.MaxAttachmentBytes
	var decoded []channel.Attachment
	add := func(att channel.Attachment) {
		if att.MediaType == "" || att.Fetch == "" {
			return
		}
		if limit <= 0 {
			att.Skip = "attachment downloads are disabled on this channel"
			att.Fetch = "" // refused: no handle is offered, nothing is fetched
		} else if att.Size > limit {
			att.Skip = fmt.Sprintf("attachment exceeds the %d-byte limit", limit)
			att.Fetch = ""
		}
		decoded = append(decoded, att)
	}

	if len(m.Photo) > 0 {
		// Sizes are ordered smallest to largest; the last is the file to use.
		best := m.Photo[len(m.Photo)-1]
		unique := best.FileUniqueID
		if unique == "" {
			unique = best.FileID
		}
		add(channel.Attachment{
			MediaType: "photo",
			MIME:      "image/jpeg",
			Size:      best.FileSize,
			Filename:  "photo-" + unique + ".jpg",
			Fetch:     best.FileID,
		})
	}
	if d := m.Document; d != nil {
		add(channel.Attachment{
			MediaType: "document",
			MIME:      d.MimeType,
			Size:      d.FileSize,
			Filename:  d.FileName,
			Fetch:     d.FileID,
		})
	}
	if v := m.Voice; v != nil {
		unique := v.FileUniqueID
		if unique == "" {
			unique = v.FileID
		}
		add(channel.Attachment{
			MediaType: "voice",
			MIME:      v.MimeType,
			Size:      v.FileSize,
			Filename:  "voice-" + unique + ".ogg",
			Fetch:     v.FileID,
		})
	}
	if v := m.Video; v != nil {
		name := v.FileName
		if name == "" {
			unique := v.FileUniqueID
			if unique == "" {
				unique = v.FileID
			}
			name = "video-" + unique + ".mp4"
		}
		add(channel.Attachment{
			MediaType: "video",
			MIME:      v.MimeType,
			Size:      v.FileSize,
			Filename:  name,
			Fetch:     v.FileID,
		})
	}
	if au := m.Audio; au != nil {
		add(channel.Attachment{
			MediaType: "audio",
			MIME:      au.MimeType,
			Size:      au.FileSize,
			Filename:  au.FileName,
			Fetch:     au.FileID,
		})
	}
	if vn := m.VideoNote; vn != nil {
		unique := vn.FileUniqueID
		if unique == "" {
			unique = vn.FileID
		}
		add(channel.Attachment{
			MediaType: "video_note",
			MIME:      "video/mp4",
			Size:      vn.FileSize,
			Filename:  "video_note-" + unique + ".mp4",
			Fetch:     vn.FileID,
		})
	}
	return decoded
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
// reply to one of the bot's messages. Media messages are gated on their
// caption, so an attachment can never bypass the group mention gate (#251).
func addressedToBot(m *tgMessage, botID int64, botUser string, mentionRe *regexp.Regexp) bool {
	if m.ReplyToMessage != nil && m.ReplyToMessage.From.ID == botID {
		return true
	}
	text, entities := m.Text, m.Entities
	if text == "" && m.Caption != "" {
		text, entities = m.Caption, m.CaptionEntities
	}
	if botUser == "" && botID == 0 {
		return false
	}
	for _, e := range entities {
		switch e.Type {
		case "mention":
			if strings.EqualFold(entityText(text, e), "@"+botUser) {
				return true
			}
		case "text_mention":
			if e.User != nil && e.User.ID == botID {
				return true
			}
		case "bot_command":
			// /cmd@botusername is how Telegram addresses a bot in groups.
			cmd := entityText(text, e)
			if i := strings.Index(cmd, "@"); i >= 0 && strings.EqualFold(cmd[i+1:], botUser) {
				return true
			}
		}
	}
	// Fallback: plain-text @username (entities missing in some proxies).
	if mentionRe != nil && mentionRe.MatchString(text) {
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
func stripBotMention(text string, mentionRe *regexp.Regexp) string {
	if mentionRe == nil {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(mentionRe.ReplaceAllString(text, ""))
}

func (a *Adapter) botIdentity() (id int64, user string, mentionRe *regexp.Regexp) {
	a.botMu.Lock()
	defer a.botMu.Unlock()
	return a.botID, a.botUser, a.botMentionRe
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
	a.botMentionRe = botMentionRegexp(me.Username)
	a.botMu.Unlock()
	if a.onPoll != nil {
		a.onPoll()
	}
	return nil
}

// botMentionRegexp compiles the case-insensitive "@user" mention pattern
// used to detect and strip bot mentions in group chats.
func botMentionRegexp(user string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(user) + `\b`)
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
			"allowed_updates": []string{"message", "edited_message"},
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
		"allowed_updates": []string{"message", "edited_message"},
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

// SendAttachment implements channel.AttachmentSender. Photos go through
// sendPhoto; documents (and only those) through sendDocument. Bytes are
// uploaded as multipart/form-data, with the caption attached when set.
func (a *Adapter) SendAttachment(ctx context.Context, chatID string, att channel.Attachment, caption string) error {
	if len(att.Data) == 0 {
		return fmt.Errorf("telegram: cannot send %s attachment with no bytes", att.MediaType)
	}
	var method, fileField, mime string
	switch att.MediaType {
	case "photo":
		method, fileField, mime = "sendPhoto", "photo", "image/jpeg"
	case "document":
		method, fileField, mime = "sendDocument", "document", "application/octet-stream"
	default:
		return fmt.Errorf("telegram: cannot send attachment of media type %q (only photo and document)", att.MediaType)
	}
	if att.MIME != "" {
		mime = att.MIME
	}
	filename := att.Filename
	if filename == "" {
		filename = "attachment.bin"
		if att.MediaType == "photo" {
			filename = "photo.jpg"
		}
	}
	fields := map[string]string{"chat_id": chatID}
	if caption != "" {
		fields["caption"] = caption
	}
	return a.upload(ctx, method, fields, fileField, filename, att.Data, mime)
}

// upload posts one multipart/form-data request to the Bot API. Used for
// attachment sends, which carry bytes rather than JSON.
func (a *Adapter) upload(ctx context.Context, method string, fields map[string]string, fileField, filename string, data []byte, mime string) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if v == "" {
			continue
		}
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, fileField, filename))
	h.Set("Content-Type", mime)
	fw, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	url := fmt.Sprintf("%s/bot%s/%s", a.baseURL, a.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := parseAPI(resp); err != nil {
		return fmt.Errorf("telegram: %s: %w", method, err)
	}
	return nil
}

// FetchAttachment implements channel.AttachmentFetcher: resolves a decoded
// attachment's handle to bytes via getFile + the file endpoint. The size cap
// is enforced before the file download (getFile's file_size) and the stream
// is additionally bounded by it, so an oversized or lying file cannot inflate
// memory. Bytes are returned in memory — no temp file is written, so there is
// no world-readable path and nothing to clean up on error or cancellation.
func (a *Adapter) FetchAttachment(ctx context.Context, handle string) ([]byte, error) {
	if a.MaxAttachmentBytes <= 0 {
		return nil, errors.New("telegram: attachment downloads are disabled")
	}
	raw, err := a.call(ctx, "getFile", map[string]any{"file_id": handle})
	if err != nil {
		return nil, err
	}
	var f struct {
		FileID   string `json:"file_id"`
		FileSize int64  `json:"file_size"`
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("telegram: parse getFile: %w", err)
	}
	if f.FilePath == "" {
		return nil, errors.New("telegram: getFile returned no file_path")
	}
	if f.FileSize > a.MaxAttachmentBytes {
		return nil, fmt.Errorf("telegram: attachment %d bytes exceeds the %d-byte limit", f.FileSize, a.MaxAttachmentBytes)
	}
	url := fmt.Sprintf("%s/file/bot%s/%s", a.baseURL, a.token, f.FilePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram: download %s: %w", f.FilePath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram: download %s: status %s", f.FilePath, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, a.MaxAttachmentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("telegram: download %s: %w", f.FilePath, err)
	}
	if int64(len(data)) > a.MaxAttachmentBytes {
		return nil, fmt.Errorf("telegram: attachment exceeds the %d-byte limit", a.MaxAttachmentBytes)
	}
	return data, nil
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

	return parseAPI(resp)
}

// parseAPI decodes a Bot API response envelope.
func parseAPI(resp *http.Response) (json.RawMessage, error) {
	var api apiResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8*1024*1024)).Decode(&api); err != nil {
		return nil, fmt.Errorf("bad response: %w", err)
	}
	if !api.OK {
		return nil, errors.New(api.Description)
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
		if i := strings.LastIndexByte(text[:limit], '\n'); i > limit/2 {
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
	if cut := len(textcut.Cut(text, limit)); cut > 0 {
		return cut
	}
	return limit // a single rune longer than limit; cut anyway to progress
}

var _ channel.Adapter = (*Adapter)(nil)
var _ channel.AttachmentFetcher = (*Adapter)(nil)
var _ channel.AttachmentSender = (*Adapter)(nil)
