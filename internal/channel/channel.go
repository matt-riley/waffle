// Package channel defines the adapter interface every messaging surface
// implements (docs/plan.md, "Entity model"). Adapters are dumb pipes:
// inbound messages go to the gateway, outbound text comes back. Routing,
// identity, and policy are the gateway's job.
package channel

import (
	"context"
	"fmt"
	"strings"
)

// Message is one inbound message from a channel.
type Message struct {
	Channel    string // adapter name, e.g. "telegram"
	ChatID     string // conversation scope (a DM, a group chat)
	SenderID   string // channel-specific sender id
	SenderName string
	// Text is the message text, or the caption for attachment-bearing
	// messages. It may be empty when the message is only an attachment.
	Text string
	// Attachments carries media decoded from the message (photo, document,
	// voice, ...). Adapters decode metadata without fetching bytes; the
	// gateway resolves each Fetch handle only after the sender has been
	// admitted (see AttachmentFetcher), so strangers' attachments are never
	// downloaded.
	Attachments []Attachment
	// IsGroup is true for multi-party chats (Telegram group/supergroup/channel).
	// Group messages are mention-gated by adapters and run on a restricted
	// agent tier; unknown senders are silently ignored (no pairing codes).
	IsGroup bool
	// ChatType is the channel-native conversation kind when known
	// (e.g. "private", "group", "supergroup", "channel").
	ChatType string
	// AckID is an opaque, adapter-scoped delivery token (#257). When set, the
	// gateway confirms the message through the adapter's Acknowledger once the
	// handler has finished, and an unconfirmed message is redelivered after a
	// restart instead of vanishing. Empty means the adapter does not track
	// delivery.
	AckID string
}

// Attachment is one media item attached to a Message. MediaType is the
// channel-native kind (e.g. "photo", "document", "voice", "video", "audio",
// "video_note" for Telegram).
type Attachment struct {
	MediaType string // channel-native media kind
	MIME      string // content type when the channel reports one
	Size      int64  // byte size as reported by the channel; the cap check happens before any fetch
	Filename  string // original file name when known; adapters may synthesize one
	// Data holds the downloaded bytes once the gateway has resolved Fetch.
	Data []byte
	// Fetch is an opaque, adapter-scoped handle (e.g. a Telegram file_id)
	// resolved through AttachmentFetcher. Empty when Data is already
	// populated or the attachment was refused (see Skip).
	Fetch string
	// Skip records why the attachment was not fetched (disabled, over the
	// size cap, download failed) so the user gets an explanation instead of
	// silence. Empty means the attachment is ready or still pending fetch.
	Skip string
}

// Acknowledger is implemented by adapters with at-least-once delivery. The
// gateway calls Ack exactly once per delivered message, after handling it —
// including when handling failed, since a handled failure has already been
// reported to the sender. A message that is never acked (the process died, or
// shutdown dropped it) stays unconfirmed at its source and arrives again.
type Acknowledger interface {
	Ack(ackID string)
}

// Adapter is one messaging surface.
type Adapter interface {
	// Name identifies the channel ("telegram", "discord", "cli").
	Name() string
	// Run blocks, delivering inbound messages until ctx is done.
	Run(ctx context.Context, inbound chan<- Message) error
	// Send delivers text to a conversation, splitting as the channel
	// requires.
	Send(ctx context.Context, chatID, text string) error
}

// AttachmentFetcher is implemented by adapters whose attachments arrive as
// decoded metadata plus a fetch handle. The gateway calls FetchAttachment
// only for senders it has identified, inside the conversation lock, so an
// unknown sender's attachment is never downloaded.
type AttachmentFetcher interface {
	// FetchAttachment resolves handle to bytes, enforcing the adapter's
	// size cap before any download. Bytes are returned in memory; adapters
	// must not leave them in world-readable locations.
	FetchAttachment(ctx context.Context, handle string) ([]byte, error)
}

// AttachmentSender is implemented by adapters that can deliver attachment
// bytes outbound. Text-only adapters stay valid by not implementing it;
// callers degrade through SendAttachmentOrExplain.
type AttachmentSender interface {
	// SendAttachment delivers one attachment with an optional caption. The
	// adapter chooses the channel-native representation (e.g. sendPhoto for
	// photos, sendDocument for documents).
	SendAttachment(ctx context.Context, chatID string, att Attachment, caption string) error
}

// SendAttachmentOrExplain delivers att to chatID through a, degrading to a
// short text explanation when the adapter cannot carry attachments. It never
// turns an attachment into a run error: a text-only channel gets the
// explanation instead.
func SendAttachmentOrExplain(ctx context.Context, a Adapter, chatID string, att Attachment, caption string) error {
	if s, ok := a.(AttachmentSender); ok {
		return s.SendAttachment(ctx, chatID, att, caption)
	}
	return a.Send(ctx, chatID, attachmentFallbackText(att, caption))
}

// attachmentFallbackText is the text that stands in for an attachment on a
// channel that cannot carry bytes.
func attachmentFallbackText(att Attachment, caption string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[attachment: %s", att.MediaType)
	if att.Filename != "" {
		fmt.Fprintf(&b, " %q", att.Filename)
	}
	if att.Size > 0 {
		fmt.Fprintf(&b, ", %d bytes", att.Size)
	}
	b.WriteString(" — this channel cannot send attachments]")
	if caption != "" {
		b.WriteString("\n" + caption)
	}
	return b.String()
}
