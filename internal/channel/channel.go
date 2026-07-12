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
