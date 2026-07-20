package main

import (
	"context"
	"strings"
	"testing"

	chatpkg "github.com/matt-riley/waffle/internal/chat"
)

func TestRunTUIChatHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	backend := &plainBackend{state: chatpkg.State{SessionID: "01TUI"}}
	err := runTUIChat(ctx, backend, chatpkg.OpenOptions{}, strings.NewReader(""), &strings.Builder{})
	if err == nil {
		t.Fatal("runTUIChat returned nil for cancelled context")
	}
}

func TestChatRendererRoutingUsesTUIOnlyForTTY(t *testing.T) {
	if !shouldRunPlain(chatOptions{}, strings.NewReader(""), &strings.Builder{}, func(int) bool { return true }) {
		t.Fatal("non-files must remain plain")
	}
}
