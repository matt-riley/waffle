package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chatpkg "github.com/matt-riley/waffle/internal/chat"
)

func TestHelpDocumentsChatFlags(t *testing.T) {
	var output bytes.Buffer
	usage(&output)

	for _, flag := range []string{"--continue", "--profile", "--socket", "--plain"} {
		if !strings.Contains(output.String(), flag) {
			t.Errorf("top-level help does not document chat flag %q:\n%s", flag, output.String())
		}
	}
}

func TestChatDocumentationMatchesCommandContract(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "chat.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	document := string(body)

	for _, command := range chatpkg.Commands() {
		if !strings.Contains(document, "`"+command.Usage+"`") {
			t.Errorf("docs/chat.md does not document canonical usage %q", command.Usage)
		}
		for _, alias := range command.Aliases {
			if !strings.Contains(document, "`/"+alias+"`") {
				t.Errorf("docs/chat.md does not document alias %q for %q", alias, command.Name)
			}
		}
	}

	for _, term := range []string{
		"/run/waffle/chat.sock",
		"waffle-chat.socket",
		"NO_COLOR",
		"direct mode",
		"does not fall back",
	} {
		if !strings.Contains(document, term) {
			t.Errorf("docs/chat.md does not document %q", term)
		}
	}
}
