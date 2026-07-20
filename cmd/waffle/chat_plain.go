package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"unicode"

	chatpkg "github.com/matt-riley/waffle/internal/chat"
)

const maxPlainInputBytes = 1 << 20

func runPlainChat(ctx context.Context, backend chatpkg.Backend, open chatpkg.OpenOptions, in io.Reader, out, stderr io.Writer) error {
	state, err := backend.Open(ctx, open)
	if err != nil {
		return err
	}
	defer func() { _ = backend.Close(context.WithoutCancel(ctx)) }()

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), maxPlainInputBytes)
	return scanPlainInput(ctx, backend, state, scanner, out, stderr)
}

func scanPlainInput(ctx context.Context, backend chatpkg.Backend, state chatpkg.State, scanner *bufio.Scanner, out, stderr io.Writer) error {
	fmt.Fprintf(out, "waffle chat — %s via %s — session %s. /help for commands.\n",
		plainRow(state.ModelAlias), plainRow(state.ProviderLabel), plainRow(state.SessionID))
	if len(state.History) > 0 {
		fmt.Fprintf(out, "(continuing with %d earlier turns)\n", len(state.History))
	}
	if state.ModelError != "" {
		fmt.Fprintf(stderr, "waffle: %s\n", plainRow(state.ModelError))
	}

	for {
		fmt.Fprint(out, "\nyou> ")
		if !scanner.Scan() {
			fmt.Fprintln(out)
			return scanner.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		emit := func(event chatpkg.Event) { renderChatEvent(event, out, stderr) }
		command, ok, parseErr := chatpkg.ParseInput(line)
		if parseErr != nil {
			fmt.Fprintf(stderr, "waffle: %s\n", plainRow(parseErr.Error()))
			continue
		}
		if ok {
			result, commandErr := backend.Command(ctx, command, emit)
			if commandErr != nil {
				fmt.Fprintf(stderr, "waffle: %s\n", plainRow(commandErr.Error()))
				continue
			}
			renderChatResult(result, out)
			if result.ShouldClose {
				return nil
			}
			continue
		}
		if turnErr := backend.Turn(ctx, line, emit); turnErr != nil {
			fmt.Fprintf(stderr, "\nwaffle: %s\n", plainRow(turnErr.Error()))
		}
	}
}

func renderChatEvent(event chatpkg.Event, stdout, stderr io.Writer) {
	switch event.Kind {
	case chatpkg.EventTextDelta:
		fmt.Fprint(stdout, event.Text)
	case chatpkg.EventToolStarted:
		fmt.Fprintf(stdout, "\n[%s]\n", plainRow(event.ToolName))
	case chatpkg.EventToolFinished:
		status := "ok"
		if event.IsError {
			status = "error"
		}
		fmt.Fprintf(stdout, "[%s -> %s, %d bytes]\n", plainRow(event.ToolName), status, event.ByteCount)
	case chatpkg.EventNotice:
		fmt.Fprintln(stderr, plainRow(event.Text))
	case chatpkg.EventTurnDone:
		fmt.Fprintln(stdout)
	}
}

func renderChatResult(result chatpkg.Result, stdout io.Writer) {
	if result.Title != "" {
		fmt.Fprintln(stdout, plainRow(result.Title))
	}
	if result.Text != "" {
		fmt.Fprintln(stdout, plainText(result.Text))
	}
	for _, command := range result.Commands {
		fmt.Fprintf(stdout, "%-58s %s\n", plainRow(command.Usage), plainRow(command.Description))
	}
	for _, model := range result.Models {
		marker := " "
		if model.Current {
			marker = "*"
		}
		fmt.Fprintf(stdout, "%s %s via %s (%s)\n", marker, plainRow(model.Alias), plainRow(model.Provider), plainRow(model.Upstream))
	}
	for _, value := range result.Sessions {
		fmt.Fprintf(stdout, "%s  %s  model=%s", plainRow(value.ID), plainRow(value.Title), plainRow(value.ModelAlias))
		if value.Summary != "" {
			fmt.Fprintf(stdout, "  summary=%s", plainRow(value.Summary))
		}
		if !value.UpdatedAt.IsZero() {
			fmt.Fprintf(stdout, "  updated=%s", value.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"))
		}
		fmt.Fprintln(stdout)
	}
	for _, value := range result.Usage {
		fmt.Fprintf(stdout, "session=%s period=%s start=%s requests=%d input=%d output=%d reserved=%d\n",
			plainRow(value.SessionID), plainRow(value.Period), plainRow(value.PeriodStart), value.Requests,
			value.InputTokens, value.OutputTokens, value.ReservedTokens)
	}
	if result.Permissions != nil {
		fmt.Fprintf(stdout, "sandbox=%s allow=%s deny=%s deny-prefixes=%s\n",
			plainRow(result.Permissions.SandboxMode), plainList(result.Permissions.Allow),
			plainList(result.Permissions.Deny), plainList(result.Permissions.DenyPrefixes))
	}
	for _, value := range result.Workset {
		fmt.Fprintf(stdout, "%s  %s\n", plainRow(value.ID), plainRow(value.Text))
	}
	if result.State != nil {
		fmt.Fprintf(stdout, "session=%s model=%s provider=%s profile=%s connection=%s sandbox=%s workspace=%s\n",
			plainRow(result.State.SessionID), plainRow(result.State.ModelAlias), plainRow(result.State.ProviderLabel),
			plainRow(result.State.Profile), plainRow(result.State.ConnectionMode), plainRow(result.State.SandboxMode),
			plainRow(result.State.Workspace))
	}
}

func plainList(values []string) string {
	rows := make([]string, len(values))
	for i, value := range values {
		rows[i] = plainRow(value)
	}
	return strings.Join(rows, ",")
}

func plainRow(value string) string {
	return strings.Join(strings.Fields(plainText(value)), " ")
}

func plainText(value string) string {
	value = stripANSI(value)
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\t':
			return r
		case '\r':
			return '\n'
		default:
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}
	}, value)
}

func stripANSI(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); {
		if value[i] != 0x1b {
			out.WriteByte(value[i])
			i++
			continue
		}
		i++
		if i >= len(value) {
			break
		}
		if value[i] != '[' {
			i++
			continue
		}
		i++
		for i < len(value) {
			last := value[i]
			i++
			if last >= 0x40 && last <= 0x7e {
				break
			}
		}
	}
	return out.String()
}
