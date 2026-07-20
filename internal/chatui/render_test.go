package chatui

import (
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/llm"
)

func TestRenderSnapshots(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	tests := []struct {
		name          string
		width, height int
		noColor       bool
		stripColor    bool
		golden        string
	}{
		{"focused", 100, 30, false, true, "focused.golden"},
		{"narrow", 58, 24, false, true, "narrow.golden"},
		{"monochrome", 100, 30, true, false, "monochrome.golden"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := snapshotModel(tt.width, tt.height, tt.noColor)
			got := m.View().Content
			if tt.stripColor {
				got = ansi.Strip(got)
			}
			want, err := os.ReadFile(filepath.Join("testdata", tt.golden))
			if err != nil {
				t.Fatalf("%v\n--- got ---\n%s\n--- end ---", err, got)
			}
			if got != strings.TrimSuffix(string(want), "\n") {
				t.Fatalf("render mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
			for _, required := range []string{"Waffle", "Focused deploy", "You", "read logs", "Ask Waffle", "/help"} {
				if !strings.Contains(ansi.Strip(got), required) {
					t.Errorf("render missing %q", required)
				}
			}
		})
	}
}

func TestModelAdaptsToTerminalBackgroundUnlessColorDisabled(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{NoColor: false})
	darkCursorLine := fmt.Sprint(m.composer.Styles().Focused.CursorLine.GetBackground())
	m = updateForTest(t, m, tea.BackgroundColorMsg{Color: color.RGBA{R: 255, G: 255, B: 255, A: 255}})
	if m.theme.dark {
		t.Fatal("white terminal background retained dark palette")
	}
	if lightCursorLine := fmt.Sprint(m.composer.Styles().Focused.CursorLine.GetBackground()); lightCursorLine == darkCursorLine {
		t.Fatalf("textarea palette did not adapt: %q", lightCursorLine)
	}
	mono := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{NoColor: true})
	mono = updateForTest(t, mono, tea.BackgroundColorMsg{Color: color.RGBA{A: 255}})
	if !mono.theme.noColor {
		t.Fatal("background response enabled disabled colors")
	}
}

func snapshotModel(width, height int, noColor bool) *Model {
	state := chat.State{SessionID: "01SNAPSHOT", Title: "Focused deploy", ModelAlias: "gpt", ConnectionMode: "local", History: []llm.Message{llm.UserText("Explain the failed deploy.")}}
	m := New(newFakeBackend(state), chat.OpenOptions{}, Options{Width: width, Height: height, NoColor: noColor})
	m.state, m.connected = state, true
	m.messages = cardsFromHistory(state.History)
	blankRows := 8
	if width < 72 {
		blankRows = 4
	}
	m.messages = append(m.messages,
		messageCard{role: roleAssistant, text: "## Finding\nThe service could not parse its **database URL**." + strings.Repeat("\n", blankRows)},
		messageCard{role: roleError, text: "Deploy remains unhealthy."},
	)
	m.tools = append(m.tools, toolRow{name: "read logs", messageIndex: 1, done: true, byteCount: 2100})
	m.overlayResult = chat.Result{Text: "Switch to the selected session and preserve all stored history before continuing?", Confirm: true}
	m.pendingConfirm = chat.ParsedCommand{Name: chat.CommandResume, Args: "01SNAPSHOT"}
	m.setOverlay(overlayConfirm, nil)
	m.syncViewport(false)
	return m
}

func TestRenderMarkdownPlainTextKeepsStructure(t *testing.T) {
	got := renderMarkdown("# Heading\n- *one* and **two**\n- [docs](https://example.com) with `code`\n```go\nfmt.Println(1)\n```", newTheme(false, true), 60)
	for _, want := range []string{"Heading", "• one and two", "docs (https://example.com) with code", "fmt.Println(1)"} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown missing %q in %q", want, got)
		}
	}
}
