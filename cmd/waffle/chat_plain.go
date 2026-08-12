package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	chatpkg "github.com/matt-riley/waffle/internal/chat"
)

const (
	maxPlainInputBytes        = 1 << 20
	maxPlainScannerTokenBytes = maxPlainInputBytes + 1
	plainCloseTimeout         = 10 * time.Second
)

func runPlainChat(ctx context.Context, backend chatpkg.Backend, open chatpkg.OpenOptions, in io.Reader, out, stderr io.Writer) error {
	renderer := newPlainRenderer(out, stderr)
	opened := false
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), plainCloseTimeout)
		defer cancel()
		closeErr := backend.Close(closeCtx)
		if opened && closeErr != nil {
			renderer.warning(closeErr.Error())
		}
	}()

	state, err := backend.Open(ctx, open)
	if err != nil {
		return err
	}
	opened = true

	scanner := bufio.NewScanner(in)
	// Scanner needs one byte beyond the logical content limit to observe EOF
	// or a trailing newline without rejecting an exactly 1 MiB token. The
	// logical length check below remains the user-visible inclusive bound.
	scanner.Buffer(make([]byte, 0, 64*1024), maxPlainScannerTokenBytes)
	return scanPlainInput(ctx, backend, state, scanner, renderer)
}

func scanPlainInput(ctx context.Context, backend chatpkg.Backend, state chatpkg.State, scanner *bufio.Scanner, renderer *plainRenderer) error {
	fmt.Fprintf(renderer.stdout, "waffle chat — %s via %s — session %s. /help for commands.\n",
		plainRow(state.ModelAlias), plainRow(state.ProviderLabel), plainRow(state.SessionID))
	if len(state.History) > 0 {
		fmt.Fprintf(renderer.stdout, "(continuing with %d earlier turns)\n", len(state.History))
	}
	if state.ModelError != "" {
		fmt.Fprintf(renderer.stderr, "waffle: %s\n", plainRow(state.ModelError))
	}

	pendingConfirmation := ""
	for {
		fmt.Fprint(renderer.stdout, "\nyou> ")
		if !scanner.Scan() {
			fmt.Fprintln(renderer.stdout)
			return scanner.Err()
		}
		if len(scanner.Bytes()) > maxPlainInputBytes {
			return fmt.Errorf("plain chat input exceeds %d bytes", maxPlainInputBytes)
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if pendingConfirmation != "" && strings.EqualFold(line, "yes") {
			fmt.Fprintf(renderer.stderr, "waffle: confirmation requires %s\n", plainConfirmationAction(pendingConfirmation))
			continue
		}

		emit := renderer.event
		command, ok, parseErr := chatpkg.ParseInput(line)
		if parseErr != nil {
			fmt.Fprintf(renderer.stderr, "waffle: %s\n", plainRow(parseErr.Error()))
			continue
		}
		if ok {
			pendingConfirmation = ""
			result, commandErr := backend.Command(ctx, command, emit)
			if commandErr != nil {
				fmt.Fprintf(renderer.stderr, "waffle: %s\n", plainRow(commandErr.Error()))
				continue
			}
			renderer.result(result)
			if result.Confirm {
				pendingConfirmation = confirmationInstruction(command)
				fmt.Fprintln(renderer.stdout, pendingConfirmation)
			}
			if result.ShouldClose {
				return nil
			}
			continue
		}
		pendingConfirmation = ""
		if turnErr := backend.Turn(ctx, line, emit); turnErr != nil {
			fmt.Fprintf(renderer.stderr, "\nwaffle: %s\n", plainRow(turnErr.Error()))
		}
	}
}

func confirmationInstruction(command chatpkg.ParsedCommand) string {
	if command.Name == chatpkg.CommandNew && command.Args == "" {
		return "confirm with /new " + chatNewConfirmArg
	}
	return "retry " + plainCommand(command) + " when idle"
}

func plainConfirmationAction(instruction string) string {
	if action, ok := strings.CutPrefix(instruction, "confirm with "); ok {
		return action
	}
	return instruction
}

func plainCommand(command chatpkg.ParsedCommand) string {
	value := "/" + string(command.Name)
	if command.Args != "" {
		value += " " + command.Args
	}
	return value
}

type plainRenderer struct {
	stdout   io.Writer
	stderr   io.Writer
	warnings map[string]struct{}
}

func newPlainRenderer(stdout, stderr io.Writer) *plainRenderer {
	return &plainRenderer{stdout: stdout, stderr: stderr, warnings: make(map[string]struct{})}
}

func renderChatEvent(event chatpkg.Event, stdout, stderr io.Writer) {
	newPlainRenderer(stdout, stderr).event(event)
}

func (r *plainRenderer) event(event chatpkg.Event) {
	switch event.Kind {
	case chatpkg.EventTextDelta:
		fmt.Fprint(r.stdout, event.Text)
	case chatpkg.EventToolStarted:
		fmt.Fprintf(r.stdout, "\n[%s]\n", plainRow(event.ToolName))
	case chatpkg.EventToolFinished:
		status := "ok"
		if event.IsError {
			status = "error"
		}
		fmt.Fprintf(r.stdout, "[%s -> %s, %d bytes]\n", plainRow(event.ToolName), status, event.ByteCount)
	case chatpkg.EventNotice:
		if event.IsError || isPlainWarning(event.Text) {
			r.warning(event.Text)
		} else {
			fmt.Fprintln(r.stderr, plainRow(event.Text))
		}
	case chatpkg.EventTurnDone:
		fmt.Fprintln(r.stdout)
	}
}

func renderChatResult(result chatpkg.Result, stdout io.Writer) {
	newPlainRenderer(stdout, io.Discard).result(result)
}

func (r *plainRenderer) result(result chatpkg.Result) {
	if result.Title != "" {
		fmt.Fprintln(r.stdout, plainRow(result.Title))
	}
	if result.Text != "" {
		r.resultText(result.Text)
	}
	for _, command := range result.Commands {
		fmt.Fprintf(r.stdout, "%-58s %s\n", plainRow(command.Usage), plainRow(command.Description))
	}
	for _, model := range result.Models {
		marker := " "
		if model.Current {
			marker = "*"
		}
		fmt.Fprintf(r.stdout, "%s %s via %s (%s)\n", marker, plainRow(model.Alias), plainRow(model.Provider), plainRow(model.Upstream))
	}
	for _, value := range result.Sessions {
		fmt.Fprintf(r.stdout, "%s  %s  model=%s", plainRow(value.ID), plainRow(value.Title), plainRow(value.ModelAlias))
		if value.Summary != "" {
			fmt.Fprintf(r.stdout, "  summary=%s", plainRow(value.Summary))
		}
		if !value.UpdatedAt.IsZero() {
			fmt.Fprintf(r.stdout, "  updated=%s", value.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"))
		}
		fmt.Fprintln(r.stdout)
	}
	for _, value := range result.Usage {
		fmt.Fprintf(r.stdout, "session=%s period=%s start=%s requests=%d input=%d cache_write=%d cache_read=%d output=%d reserved=%d\n",
			plainRow(value.SessionID), plainRow(value.Period), plainRow(value.PeriodStart), value.Requests,
			value.InputTokens, value.CacheCreationInputTokens, value.CacheReadInputTokens, value.OutputTokens, value.ReservedTokens)
	}
	if result.Permissions != nil {
		fmt.Fprintf(r.stdout, "sandbox=%s allow=%s deny=%s deny-prefixes=%s\n",
			plainRow(result.Permissions.SandboxMode), plainList(result.Permissions.Allow),
			plainList(result.Permissions.Deny), plainList(result.Permissions.DenyPrefixes))
	}
	for _, value := range result.Workset {
		fmt.Fprintf(r.stdout, "%s  %s\n", plainRow(value.ID), plainRow(value.Text))
	}
	if result.State != nil {
		fmt.Fprintf(r.stdout, "session=%s model=%s provider=%s profile=%s connection=%s sandbox=%s workspace=%s\n",
			plainRow(result.State.SessionID), plainRow(result.State.ModelAlias), plainRow(result.State.ProviderLabel),
			plainRow(result.State.Profile), plainRow(result.State.ConnectionMode), plainRow(result.State.SandboxMode),
			plainRow(result.State.Workspace))
	}
}

func (r *plainRenderer) resultText(text string) {
	sanitized := plainText(text)
	first, rest, found := strings.Cut(sanitized, "\n")
	if !isPlainWarning(first) {
		fmt.Fprintln(r.stdout, sanitized)
		return
	}
	if warning := normalizePlainWarning(sanitized); warning != "" {
		if _, alreadyRendered := r.warnings[warning]; alreadyRendered {
			return
		}
	}
	r.warning(first)
	if found && rest != "" {
		fmt.Fprintln(r.stdout, rest)
	}
}

func (r *plainRenderer) warning(message string) {
	message = normalizePlainWarning(message)
	if message == "" {
		return
	}
	if _, exists := r.warnings[message]; exists {
		return
	}
	r.warnings[message] = struct{}{}
	fmt.Fprintf(r.stderr, "waffle: warning: %s\n", message)
}

func isPlainWarning(message string) bool {
	message = strings.TrimSpace(plainRow(message))
	return hasPrefixFold(message, "warning:") || hasPrefixFold(message, "waffle: warning:")
}

func normalizePlainWarning(message string) string {
	message = strings.TrimSpace(plainRow(message))
	if rest, ok := cutPrefixFold(message, "waffle:"); ok {
		message = strings.TrimSpace(rest)
	}
	if rest, ok := cutPrefixFold(message, "warning:"); ok {
		message = strings.TrimSpace(rest)
	}
	return message
}

func hasPrefixFold(value, prefix string) bool {
	_, ok := cutPrefixFold(value, prefix)
	return ok
}

func cutPrefixFold(value, prefix string) (string, bool) {
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return value, false
	}
	return value[len(prefix):], true
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
