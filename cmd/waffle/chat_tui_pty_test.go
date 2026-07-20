//go:build !windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
	chatpkg "github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/chatwire"
	"github.com/matt-riley/waffle/internal/llm"
)

const ptyExpectTimeout = 5 * time.Second

var (
	testBinaryOnce sync.Once
	testBinaryPath string
	testBinaryDir  string
	testBinaryErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if testBinaryDir != "" {
		_ = os.RemoveAll(testBinaryDir)
	}
	os.Exit(code)
}

func TestChatTUIPTYConversationResizeCancelAndExit(t *testing.T) {
	backend := newPTYBackend()
	server := startPTYChatServer(t, backend)
	terminal := startWafflePTY(t, server.path, 100, 30)

	terminal.expectText(t, "Focused Conversation")
	terminal.expectText(t, "local service")
	terminal.write(t, "/help\r")
	terminal.expectText(t, "Help")
	terminal.expectText(t, "show commands and keys")
	helpMark := terminal.rawLen()
	terminal.write(t, "\x1b")
	terminal.expectOutputAfter(t, helpMark)

	terminal.write(t, "/model\r")
	terminal.expectText(t, "Models")
	terminal.expectText(t, "gpt")
	terminal.write(t, "\r")
	terminal.expectText(t, "model set to gpt")

	terminal.write(t, "synthetic turn\r")
	terminal.expectText(t, "Synthetic PTY answer.")

	mark := terminal.rawLen()
	if err := pty.Setsize(terminal.file, &pty.Winsize{Rows: 24, Cols: 58}); err != nil {
		t.Fatal(err)
	}
	terminal.expectTextAfter(t, mark, "Waffle")
	terminal.expectTextAfter(t, mark, "local service")

	terminal.write(t, "delayed turn\r")
	select {
	case <-backend.delayStarted:
	case <-time.After(ptyExpectTimeout):
		t.Fatalf("delayed turn did not reach backend\n%s", terminal.failureBuffer())
	}
	terminal.write(t, "\x1b")
	terminal.expectText(t, "Turn cancelled.")

	terminal.write(t, "/exit\r")
	terminal.wait(t, expectExitZero)
	terminal.expectAlternateScreenRestored(t)
	terminal.assertNoCanaries(t)
}

func TestChatTUIPTYBackendDisconnectRestoresTerminal(t *testing.T) {
	server := startPTYChatServer(t, newPTYBackend())
	terminal := startWafflePTY(t, server.path, 100, 30)
	terminal.expectText(t, "Focused Conversation")
	server.closeClient(t)
	terminal.write(t, "detect disconnect\r")
	terminal.expectText(t, "Connection lost:")
	terminal.expectText(t, "Press Enter to close.")
	terminal.write(t, "\r")
	terminal.wait(t, expectDisconnectNonzero)
	terminal.expectAlternateScreenRestored(t)
	terminal.assertNoCanaries(t)
}

func TestChatTUIPTYSIGINTRestoresTerminal(t *testing.T) {
	server := startPTYChatServer(t, newPTYBackend())
	terminal := startWafflePTY(t, server.path, 100, 30)
	terminal.expectText(t, "Focused Conversation")
	if err := terminal.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	terminal.wait(t, expectSIGINTNonzero)
	terminal.expectAlternateScreenRestored(t)
	terminal.assertNoCanaries(t)
}

type ptyBackend struct {
	mu           sync.Mutex
	state        chatpkg.State
	delayStarted chan struct{}
	delayOnce    sync.Once
}

func newPTYBackend() *ptyBackend {
	return &ptyBackend{
		state: chatpkg.State{
			SessionID:      "01PTYSESSION",
			Title:          "Focused Conversation",
			ModelAlias:     "writer",
			ProviderLabel:  "deterministic (test)",
			Profile:        "main",
			ConnectionMode: "direct",
			SandboxMode:    "deny",
			Models: []chatpkg.Model{
				{Alias: "gpt", Provider: "deterministic", Upstream: "gpt-test"},
				{Alias: "writer", Provider: "deterministic", Upstream: "writer-test", Current: true},
			},
		},
		delayStarted: make(chan struct{}),
	}
}

func (b *ptyBackend) Open(context.Context, chatpkg.OpenOptions) (chatpkg.State, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return copyPTYState(b.state), nil
}

func (b *ptyBackend) Turn(ctx context.Context, input string, emit func(chatpkg.Event)) error {
	if input == "delayed turn" {
		b.delayOnce.Do(func() { close(b.delayStarted) })
		<-ctx.Done()
		return ctx.Err()
	}
	b.mu.Lock()
	state := copyPTYState(b.state)
	b.mu.Unlock()
	if emit != nil {
		emit(chatpkg.Event{Kind: chatpkg.EventTextDelta, Text: "Synthetic PTY answer."})
		emit(chatpkg.Event{Kind: chatpkg.EventTurnDone, Usage: llm.Usage{InputTokens: 2, OutputTokens: 3}, State: &state})
	}
	return nil
}

func (b *ptyBackend) Command(_ context.Context, command chatpkg.ParsedCommand, _ func(chatpkg.Event)) (chatpkg.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch command.Name {
	case chatpkg.CommandHelp:
		return chatpkg.Result{Title: "Chat commands", Commands: chatpkg.Commands()}, nil
	case chatpkg.CommandModel:
		if command.Args == "" {
			return chatpkg.Result{Title: "Choose a model", Models: append([]chatpkg.Model(nil), b.state.Models...)}, nil
		}
		if command.Args != "gpt" && command.Args != "writer" {
			return chatpkg.Result{}, errors.New("unknown model alias")
		}
		b.state.ModelAlias = command.Args
		for i := range b.state.Models {
			b.state.Models[i].Current = b.state.Models[i].Alias == command.Args
		}
		state := copyPTYState(b.state)
		return chatpkg.Result{Text: "model set to " + command.Args, State: &state}, nil
	case chatpkg.CommandExit:
		return chatpkg.Result{ShouldClose: true}, nil
	case chatpkg.CommandStatus:
		state := copyPTYState(b.state)
		return chatpkg.Result{Title: "Chat status", State: &state}, nil
	default:
		return chatpkg.Result{}, fmt.Errorf("unsupported test command %q", command.Name)
	}
}

func (b *ptyBackend) Cancel()                     {}
func (b *ptyBackend) Close(context.Context) error { return nil }

func copyPTYState(state chatpkg.State) chatpkg.State {
	state.Models = append([]chatpkg.Model(nil), state.Models...)
	return state
}

type ptyChatServer struct {
	path     string
	listener *trackingUnixListener
	cancel   context.CancelFunc
	done     chan error
}

func startPTYChatServer(t *testing.T, backend chatpkg.Backend) *ptyChatServer {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "waffle-pty-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "chat.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}
	tracked := &trackingUnixListener{Listener: listener, accepted: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- chatwire.Serve(ctx, tracked, func(context.Context) (chatpkg.Backend, error) { return backend, nil }, nil)
	}()
	server := &ptyChatServer{path: path, listener: tracked, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		_ = tracked.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("PTY chat server: %v", err)
			}
		case <-time.After(ptyExpectTimeout):
			t.Error("PTY chat server did not stop within five seconds")
		}
		_ = os.RemoveAll(dir)
	})
	return server
}

func (s *ptyChatServer) closeClient(t *testing.T) {
	t.Helper()
	select {
	case <-s.listener.accepted:
	case <-time.After(ptyExpectTimeout):
		t.Fatal("PTY chat server accepted no client within five seconds")
	}
	s.listener.mu.Lock()
	conn := s.listener.conn
	s.listener.mu.Unlock()
	if conn == nil {
		t.Fatal("PTY chat server has no tracked client")
	}
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}

type trackingUnixListener struct {
	net.Listener
	mu       sync.Mutex
	conn     net.Conn
	accepted chan struct{}
	once     sync.Once
}

func (l *trackingUnixListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.conn = conn
	l.mu.Unlock()
	l.once.Do(func() { close(l.accepted) })
	return conn, nil
}

type ptyProcess struct {
	cmd      *exec.Cmd
	file     *os.File
	canaries []string
	mu       sync.Mutex
	raw      bytes.Buffer
	readDone chan struct{}
	waiter   *processWait
}

func startWafflePTY(t *testing.T, socket string, cols, rows uint16) *ptyProcess {
	t.Helper()
	blockedParent := t.TempDir()
	if err := os.Chmod(blockedParent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blockedParent, 0o700) })
	cmd := exec.Command(waffleProcessBinary(t), "chat", "--socket", socket)
	cmd.Env = cleanProcessEnv(map[string]string{
		"WAFFLE_HOME":         filepath.Join(blockedParent, "nonexistent-client-home"),
		"WAFFLE_AGE_IDENTITY": clientIdentityCanary,
		"OPENAI_API_KEY":      "",
		"NO_COLOR":            "1",
		"TERM":                "xterm-256color",
	})
	file, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		t.Fatal(err)
	}
	p := &ptyProcess{
		cmd: cmd, file: file,
		canaries: []string{serverCredentialCanary, clientIdentityCanary, socket, blockedParent, filepath.Join(blockedParent, "nonexistent-client-home")},
		readDone: make(chan struct{}), waiter: waitForProcess(cmd),
	}
	go p.readLoop()
	t.Cleanup(func() {
		select {
		case <-p.waiter.done:
		default:
			_ = cmd.Process.Kill()
			select {
			case <-p.waiter.done:
			case <-time.After(ptyExpectTimeout):
			}
		}
		_ = file.Close()
	})
	return p
}

func (p *ptyProcess) readLoop() {
	defer close(p.readDone)
	buffer := make([]byte, 4096)
	for {
		n, err := p.file.Read(buffer)
		if n > 0 {
			p.mu.Lock()
			_, _ = p.raw.Write(buffer[:n])
			p.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (p *ptyProcess) write(t *testing.T, value string) {
	t.Helper()
	if _, err := io.WriteString(p.file, value); err != nil {
		t.Fatalf("write PTY input: %v\n%s", err, p.failureBuffer())
	}
}

func (p *ptyProcess) rawLen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.raw.Len()
}

func (p *ptyProcess) rawBytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.raw.Bytes()...)
}

func (p *ptyProcess) expectText(t *testing.T, needle string) {
	t.Helper()
	p.expect(t, 0, func(raw []byte) bool { return strings.Contains(sanitizePTYOutput(raw), needle) }, fmt.Sprintf("text %q", needle))
}

func (p *ptyProcess) expectTextAfter(t *testing.T, offset int, needle string) {
	t.Helper()
	p.expect(t, offset, func(raw []byte) bool { return strings.Contains(sanitizePTYOutput(raw), needle) }, fmt.Sprintf("text %q after byte %d", needle, offset))
}

func (p *ptyProcess) expectOutputAfter(t *testing.T, offset int) {
	t.Helper()
	p.expect(t, offset, func(raw []byte) bool { return len(raw) > 0 }, fmt.Sprintf("terminal redraw after byte %d", offset))
}

func (p *ptyProcess) assertNoCanaries(t *testing.T) {
	t.Helper()
	raw := string(p.rawBytes())
	sanitized := sanitizePTYOutput([]byte(raw))
	for _, canary := range p.canaries {
		if canary != "" && (strings.Contains(raw, canary) || strings.Contains(sanitized, canary)) {
			t.Errorf("PTY output leaked canary %q\n%s", canary, p.failureBuffer())
		}
	}
}

func (p *ptyProcess) expectAlternateScreenRestored(t *testing.T) {
	t.Helper()
	p.expect(t, 0, func(raw []byte) bool {
		return bytes.Contains(raw, []byte("\x1b[?1049l")) || bytes.Contains(raw, []byte("\x1b[?1047l"))
	}, "alternate-screen restore sequence")
}

func (p *ptyProcess) expect(t *testing.T, offset int, predicate func([]byte) bool, description string) {
	t.Helper()
	deadline := time.Now().Add(ptyExpectTimeout)
	for time.Now().Before(deadline) {
		raw := p.rawBytes()
		if offset <= len(raw) && predicate(raw[offset:]) {
			return
		}
		select {
		case <-p.waiter.done:
			t.Fatalf("process exited while waiting for %s: %v\n%s", description, p.waiter.err, p.failureBuffer())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after five seconds waiting for %s\n%s", description, p.failureBuffer())
}

type ptyExitExpectation uint8

const (
	expectExitZero ptyExitExpectation = iota
	expectDisconnectNonzero
	expectSIGINTNonzero
)

func (p *ptyProcess) wait(t *testing.T, expectation ptyExitExpectation) {
	t.Helper()
	select {
	case <-p.waiter.done:
		switch expectation {
		case expectExitZero:
			if p.waiter.err != nil {
				t.Fatalf("PTY process exit = %v, want zero\n%s", p.waiter.err, p.failureBuffer())
			}
		case expectDisconnectNonzero, expectSIGINTNonzero:
			var exitErr *exec.ExitError
			if !errors.As(p.waiter.err, &exitErr) || exitErr.ExitCode() == 0 {
				t.Fatalf("PTY process exit = %v, want documented nonzero status for %v\n%s", p.waiter.err, expectation, p.failureBuffer())
			}
		default:
			t.Fatalf("unknown PTY exit expectation %d", expectation)
		}
	case <-time.After(ptyExpectTimeout):
		t.Fatalf("PTY process did not exit within five seconds\n%s", p.failureBuffer())
	}
	select {
	case <-p.readDone:
	case <-time.After(ptyExpectTimeout):
		t.Fatalf("PTY reader did not stop within five seconds\n%s", p.failureBuffer())
	}
}

func (p *ptyProcess) failureBuffer() string {
	value := sanitizePTYOutput(p.rawBytes())
	const limit = 12 * 1024
	if len(value) > limit {
		value = value[len(value)-limit:]
	}
	return "sanitized PTY buffer:\n" + value
}

func sanitizePTYOutput(raw []byte) string {
	stripped := ansi.Strip(string(raw))
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, stripped)
}

func waffleProcessBinary(t *testing.T) string {
	t.Helper()
	testBinaryOnce.Do(func() {
		testBinaryDir, testBinaryErr = os.MkdirTemp("", "waffle-process-test-")
		if testBinaryErr != nil {
			return
		}
		root, err := findModuleRoot()
		if err != nil {
			testBinaryErr = err
			return
		}
		testBinaryPath = filepath.Join(testBinaryDir, "waffle")
		build := exec.Command("go", "build", "-o", testBinaryPath, "./cmd/waffle")
		build.Dir = root
		if output, err := build.CombinedOutput(); err != nil {
			testBinaryErr = fmt.Errorf("build process-test waffle: %w\n%s", err, output)
		}
	})
	if testBinaryErr != nil {
		t.Fatal(testBinaryErr)
	}
	return testBinaryPath
}

func findModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not find go.mod")
		}
		dir = parent
	}
}
