package channel

import (
	"context"
	"strings"
	"testing"
)

// textOnlyAdapter is a minimal, attachment-less channel implementation: the
// contract must keep accepting it (#251, interface compatibility).
type textOnlyAdapter struct {
	name string
	sent []string
}

func (t *textOnlyAdapter) Name() string { return t.name }

func (t *textOnlyAdapter) Run(ctx context.Context, inbound chan<- Message) error {
	<-ctx.Done()
	return nil
}

func (t *textOnlyAdapter) Send(_ context.Context, chatID, text string) error {
	t.sent = append(t.sent, text)
	return nil
}

// TestBehaviorTextOnlyAdapterStillSatisfiesContract pins that the Adapter
// contract stays satisfied by implementations that only speak text: the
// attachment surface must remain an optional capability, not a method that
// breaks every existing channel.
func TestBehaviorTextOnlyAdapterStillSatisfiesContract(t *testing.T) {
	var _ Adapter = (*textOnlyAdapter)(nil)
	a := &textOnlyAdapter{name: "cli"}
	if a.Name() != "cli" {
		t.Fatalf("name = %q", a.Name())
	}
	// Runtime assertion mirrors the compile-time one: the adapter is
	// usable wherever an Adapter is expected, including attachment sends.
	if _, ok := any(a).(AttachmentSender); ok {
		t.Fatal("text-only adapter must not claim attachment sending")
	}
	if err := SendAttachmentOrExplain(context.Background(), a, "chat-1",
		Attachment{MediaType: "photo", Filename: "x.png", Size: 42},
		"look at this"); err != nil {
		t.Fatalf("SendAttachmentOrExplain: %v", err)
	}
	if len(a.sent) != 1 {
		t.Fatalf("sent = %v, want one degraded text message", a.sent)
	}
	text := a.sent[0]
	if !strings.Contains(text, "photo") || !strings.Contains(text, "x.png") {
		t.Errorf("degraded text = %q, want media type and filename", text)
	}
	if !strings.Contains(text, "cannot send attachments") {
		t.Errorf("degraded text = %q, want clear explanation", text)
	}
	if !strings.Contains(text, "look at this") {
		t.Errorf("degraded text = %q, want caption preserved", text)
	}
}

// attachmentSenderAdapter implements the optional AttachmentSender.
type attachmentSenderAdapter struct {
	textOnlyAdapter
	gotChatID string
	gotAtt    Attachment
	gotCap    string
}

func (a *attachmentSenderAdapter) SendAttachment(_ context.Context, chatID string, att Attachment, caption string) error {
	a.gotChatID, a.gotAtt, a.gotCap = chatID, att, caption
	return nil
}

// TestBehaviorAttachmentSenderDelegates pins that SendAttachmentOrExplain
// uses the adapter's native attachment path when one exists.
func TestBehaviorAttachmentSenderDelegates(t *testing.T) {
	a := &attachmentSenderAdapter{}
	att := Attachment{MediaType: "document", Filename: "report.pdf", Size: 7}
	if err := SendAttachmentOrExplain(context.Background(), a, "chat-9", att, "the report"); err != nil {
		t.Fatalf("SendAttachmentOrExplain: %v", err)
	}
	if a.gotChatID != "chat-9" || a.gotCap != "the report" {
		t.Fatalf("delegate = %q %q", a.gotChatID, a.gotCap)
	}
	if a.gotAtt.MediaType != "document" || a.gotAtt.Filename != "report.pdf" {
		t.Fatalf("att = %+v", a.gotAtt)
	}
	if len(a.sent) != 0 {
		t.Fatalf("text-only Send used despite AttachmentSender: %v", a.sent)
	}
}
