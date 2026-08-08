// Package channel defines the adapter interface every messaging surface
// implements (docs/plan.md, "Entity model"). Adapters are dumb pipes:
// inbound messages go to the gateway, outbound text comes back. Routing,
// identity, and policy are the gateway's job.
package channel

import "context"

// Message is one inbound message from a channel.
type Message struct {
	Channel    string // adapter name, e.g. "telegram"
	ChatID     string // conversation scope (a DM, a group chat)
	SenderID   string // channel-specific sender id
	SenderName string
	Text       string
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
