package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
)

const (
	defaultChatClientLimit = 64
	defaultChatIdleTTL     = 30 * time.Minute
	chatIDAttempts         = 8
)

var (
	errChatClientNotFound = errors.New("chat_client_not_found")
	errChatTurnActive     = errors.New("turn_active")
	errChatUnavailable    = errors.New("chat_unavailable")
)

var dashboardSensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`AGE-SECRET-KEY-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`/var/lib/waffle(?:/[A-Za-z0-9._/-]+)?`),
}

// BackendFactory builds one isolated chat backend for each browser client.
type BackendFactory func(context.Context) (chat.Backend, error)

type chatClient struct {
	backend         chat.Backend
	lastActive      time.Time
	busy            bool
	done            chan struct{}
	operationCancel context.CancelFunc
	lifecycle       sync.Mutex
	cancelDone      chan struct{}
	cancelled       bool
	closeDone       chan struct{}
	closeErr        error
	closing         bool
	closed          bool
}

func (c *chatClient) prepareCancel() (chan struct{}, bool) {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	if c.closed || c.closing {
		return nil, false
	}
	if c.cancelled {
		return c.cancelDone, false
	}
	c.cancelled = true
	c.cancelDone = make(chan struct{})
	return c.cancelDone, true
}

func (c *chatClient) finishCancel(done chan struct{}) {
	c.backend.Cancel()
	close(done)
}

func (c *chatClient) close(ctx context.Context) error {
	for {
		c.lifecycle.Lock()
		if c.closed {
			err := c.closeErr
			c.lifecycle.Unlock()
			return err
		}
		if c.closing {
			done := c.closeDone
			c.lifecycle.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if c.cancelDone != nil {
			select {
			case <-c.cancelDone:
			default:
				done := c.cancelDone
				c.lifecycle.Unlock()
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		c.closing = true
		c.closeDone = make(chan struct{})
		done := c.closeDone
		c.lifecycle.Unlock()

		err := c.backend.Close(ctx)
		c.lifecycle.Lock()
		c.closeErr = err
		c.closed = true
		c.closing = false
		close(done)
		c.lifecycle.Unlock()
		return err
	}
}

func (c *chatClient) beginOperation() bool {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	if c.closed || c.closing {
		return false
	}
	if c.cancelDone != nil {
		select {
		case <-c.cancelDone:
		default:
			return false
		}
	}
	c.cancelled = false
	c.cancelDone = nil
	return true
}

// ChatClients adapts browser client IDs to isolated chat backends.
type ChatClients struct {
	mu           sync.Mutex
	clients      map[string]*chatClient
	factory      BackendFactory
	ids          io.Reader
	now          func() time.Time
	maxClients   int
	idleTTL      time.Duration
	shutdownTTL  time.Duration
	events       *EventHub
	shutting     bool
	shutdownDone chan struct{}
	shutdownErr  error
	pending      map[*chatClient]*chatCleanup
}

// NewChatClients returns a bounded manager with production lifecycle limits.
func NewChatClients(factory BackendFactory, ids io.Reader) *ChatClients {
	if ids == nil {
		ids = rand.Reader
	}
	return &ChatClients{
		clients:     make(map[string]*chatClient),
		factory:     factory,
		ids:         ids,
		now:         time.Now,
		maxClients:  defaultChatClientLimit,
		idleTTL:     defaultChatIdleTTL,
		shutdownTTL: 5 * time.Second,
		pending:     make(map[*chatClient]*chatCleanup),
	}
}

// SetEventHub attaches the process-wide Desk event hub before clients open.
func (c *ChatClients) SetEventHub(events *EventHub) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = events
}

// Open creates and opens one isolated backend for a browser client.
func (c *ChatClients) Open(ctx context.Context, options chat.OpenOptions) (string, chat.State, error) {
	if err := c.reap(ctx); err != nil {
		return "", chat.State{}, err
	}
	c.mu.Lock()
	if c.shutting || c.factory == nil || len(c.clients) >= c.maxClients {
		c.mu.Unlock()
		return "", chat.State{}, errChatUnavailable
	}
	c.mu.Unlock()

	backend, err := c.factory(ctx)
	if err != nil || backend == nil {
		return "", chat.State{}, errChatUnavailable
	}
	state, err := backend.Open(ctx, options)
	if err != nil {
		closeBackend(ctx, backend)
		return "", chat.State{}, err
	}
	c.mu.Lock()
	if c.shutting || len(c.clients) >= c.maxClients {
		c.mu.Unlock()
		closeBackend(ctx, backend)
		return "", chat.State{}, errChatUnavailable
	}
	var clientID string
	for attempts := 0; attempts < chatIDAttempts; attempts++ {
		candidate, idErr := c.newClientID()
		if idErr != nil {
			c.mu.Unlock()
			closeBackend(ctx, backend)
			return "", chat.State{}, errChatUnavailable
		}
		if _, exists := c.clients[candidate]; !exists {
			clientID = candidate
			break
		}
	}
	if clientID == "" {
		c.mu.Unlock()
		closeBackend(ctx, backend)
		return "", chat.State{}, errChatUnavailable
	}
	c.clients[clientID] = &chatClient{backend: backend, lastActive: c.now()}
	c.mu.Unlock()
	return clientID, state, nil
}

// Turn forwards exactly one client turn; callers must not retry after a disconnect.
func (c *ChatClients) Turn(ctx context.Context, clientID, input string) error {
	if err := c.reap(ctx); err != nil {
		return err
	}
	client, operationCtx, err := c.begin(ctx, clientID)
	if err != nil {
		return err
	}
	defer c.end(clientID, client)
	return client.backend.Turn(operationCtx, input, c.emit(clientID))
}

// Command forwards one command while preserving the one-active-operation invariant.
func (c *ChatClients) Command(ctx context.Context, clientID string, command chat.ParsedCommand) (chat.Result, error) {
	if err := c.reap(ctx); err != nil {
		return chat.Result{}, err
	}
	client, operationCtx, err := c.begin(ctx, clientID)
	if err != nil {
		return chat.Result{}, err
	}
	defer c.end(clientID, client)
	return client.backend.Command(operationCtx, command, c.emit(clientID))
}

// Cancel cancels a client backend without holding the manager lock.
func (c *ChatClients) Cancel(clientID string) error {
	c.mu.Lock()
	client, ok := c.clients[clientID]
	if ok {
		client.lastActive = c.now()
		if client.operationCancel != nil {
			client.operationCancel()
		}
	}
	if !ok {
		c.mu.Unlock()
		return errChatClientNotFound
	}
	done, run := client.prepareCancel()
	c.mu.Unlock()
	if run {
		client.finishCancel(done)
		return nil
	}
	if done != nil {
		<-done
	}
	return nil
}

// Close removes a client and closes its backend once active work has settled.
func (c *ChatClients) Close(ctx context.Context, clientID string) error {
	c.mu.Lock()
	client, ok := c.clients[clientID]
	var done <-chan struct{}
	var operationCancel context.CancelFunc
	if ok {
		delete(c.clients, clientID)
		if client.busy {
			done = client.done
			operationCancel = client.operationCancel
		}
	}
	if !ok {
		c.mu.Unlock()
		return errChatClientNotFound
	}
	cleanup := c.startCleanupLocked(client, done, operationCancel, ctx)
	c.mu.Unlock()
	select {
	case <-cleanup.done:
		return cleanup.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown stops future opens and closes all live backends exactly once.
func (c *ChatClients) Shutdown(ctx context.Context) error {
	closeCtx, closeCancel := detachedTimeoutContext(ctx, c.shutdownTTL)
	defer closeCancel()
	c.mu.Lock()
	if c.shutting {
		done := c.shutdownDone
		c.mu.Unlock()
		select {
		case <-done:
			c.mu.Lock()
			err := c.shutdownErr
			c.mu.Unlock()
			return err
		case <-closeCtx.Done():
			return closeCtx.Err()
		}
	}
	c.shutting = true
	c.shutdownDone = make(chan struct{})
	cleanups := make([]*chatCleanup, 0, len(c.pending)+len(c.clients))
	for _, cleanup := range c.pending {
		cleanups = append(cleanups, cleanup)
	}
	for _, client := range c.clients {
		var done <-chan struct{}
		var operationCancel context.CancelFunc
		if client.busy {
			done = client.done
			operationCancel = client.operationCancel
		}
		cleanups = append(cleanups, c.startCleanupLocked(client, done, operationCancel, closeCtx))
	}
	c.clients = make(map[string]*chatClient)
	c.mu.Unlock()

	var first error
	deadlineReached := false
	for _, cleanup := range cleanups {
		select {
		case <-cleanup.done:
			if cleanup.err != nil && first == nil {
				first = cleanup.err
			}
		case <-closeCtx.Done():
			if first == nil {
				first = closeCtx.Err()
			}
			for _, pending := range cleanups {
				pending.cancel()
			}
			deadlineReached = true
		}
		if deadlineReached {
			break
		}
	}
	if deadlineReached {
		for _, cleanup := range cleanups {
			<-cleanup.done
			if cleanup.err != nil && first == nil {
				first = cleanup.err
			}
		}
	}
	c.mu.Lock()
	c.shutdownErr = first
	close(c.shutdownDone)
	c.mu.Unlock()
	return first
}

func (c *ChatClients) begin(ctx context.Context, clientID string) (*chatClient, context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	client, ok := c.clients[clientID]
	if !ok {
		return nil, nil, errChatClientNotFound
	}
	if client.busy {
		return nil, nil, errChatTurnActive
	}
	if !client.beginOperation() {
		return nil, nil, errChatTurnActive
	}
	operationCtx, operationCancel := context.WithCancel(ctx)
	client.busy = true
	client.done = make(chan struct{})
	client.operationCancel = operationCancel
	client.lastActive = c.now()
	return client, operationCtx, nil
}

func (c *ChatClients) end(clientID string, client *chatClient) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if client.done != nil {
		close(client.done)
		client.done = nil
	}
	if client.operationCancel != nil {
		client.operationCancel()
		client.operationCancel = nil
	}
	if c.clients[clientID] == client {
		client.busy = false
		client.lastActive = c.now()
	}
}

func (c *ChatClients) reap(ctx context.Context) error {
	c.mu.Lock()
	var cleanups []*chatCleanup
	for id, client := range c.clients {
		if !client.busy && c.now().Sub(client.lastActive) >= c.idleTTL {
			delete(c.clients, id)
			cleanups = append(cleanups, c.startCleanupLocked(client, nil, nil, ctx))
		}
	}
	c.mu.Unlock()
	var first error
	for _, cleanup := range cleanups {
		select {
		case <-cleanup.done:
			if cleanup.err != nil && first == nil {
				first = cleanup.err
			}
		case <-ctx.Done():
			if first == nil {
				first = ctx.Err()
			}
		}
	}
	return first
}

func (c *ChatClients) startCleanupLocked(
	client *chatClient,
	activeDone <-chan struct{},
	operationCancel context.CancelFunc,
	parent context.Context,
) *chatCleanup {
	if cleanup, ok := c.pending[client]; ok {
		return cleanup
	}
	cleanupCtx, cancel := detachedTimeoutContext(parent, c.shutdownTTL)
	cleanup := &chatCleanup{done: make(chan struct{}), cancel: cancel}
	c.pending[client] = cleanup
	go func() {
		defer cancel()
		if operationCancel != nil {
			operationCancel()
		}
		if activeDone != nil {
			select {
			case <-activeDone:
			case <-cleanupCtx.Done():
				cleanup.err = cleanupCtx.Err()
			}
		}
		if cleanup.err == nil {
			cleanup.err = client.close(cleanupCtx)
		}
		c.mu.Lock()
		delete(c.pending, client)
		c.mu.Unlock()
		close(cleanup.done)
	}()
	return cleanup
}

func (c *ChatClients) newClientID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := io.ReadFull(c.ids, bytes); err != nil {
		return "", fmt.Errorf("read client id: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

func (c *ChatClients) emit(clientID string) func(chat.Event) {
	return func(event chat.Event) {
		if c.events == nil {
			return
		}
		if !dashboardEventKind(event.Kind) {
			return
		}
		text := sanitizeDashboardString(event.Text)
		if event.IsError {
			text = "chat operation failed"
		}
		var state *dashboardChatState
		if event.State != nil {
			safe := safeDashboardChatState(*event.State)
			state = &safe
		}
		data, err := json.Marshal(dashboardChatEvent{
			Kind:      event.Kind,
			Text:      text,
			ToolName:  sanitizeDashboardString(event.ToolName),
			IsError:   event.IsError,
			ByteCount: event.ByteCount,
			State:     state,
			Usage: dashboardUsage{
				InputTokens:  event.Usage.InputTokens,
				OutputTokens: event.Usage.OutputTokens,
			},
		})
		if err != nil {
			return
		}
		c.events.Publish(Event{Type: string(event.Kind), Resource: "chat", ResourceID: clientID, Data: data})
	}
}

type dashboardChatEvent struct {
	Kind      chat.EventKind      `json:"kind"`
	Text      string              `json:"text,omitempty"`
	ToolName  string              `json:"tool_name,omitempty"`
	IsError   bool                `json:"is_error,omitempty"`
	ByteCount int                 `json:"byte_count,omitempty"`
	Usage     dashboardUsage      `json:"usage,omitempty"`
	State     *dashboardChatState `json:"state,omitempty"`
}

type dashboardChatState struct {
	SessionID      string       `json:"session_id"`
	Title          string       `json:"title"`
	ModelAlias     string       `json:"model_alias"`
	ModelError     string       `json:"model_error"`
	ProviderLabel  string       `json:"provider_label"`
	Profile        string       `json:"profile"`
	ConnectionMode string       `json:"connection_mode"`
	SandboxMode    string       `json:"sandbox_mode"`
	Workspace      string       `json:"workspace"`
	Models         []chat.Model `json:"models"`
	Capabilities   []string     `json:"capabilities"`
}

type chatCleanup struct {
	done   chan struct{}
	cancel context.CancelFunc
	err    error
}

type dashboardUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func dashboardEventKind(kind chat.EventKind) bool {
	switch kind {
	case chat.EventTextDelta, chat.EventToolStarted, chat.EventToolFinished, chat.EventNotice, chat.EventState, chat.EventTurnDone:
		return true
	default:
		return false
	}
}

func sanitizeDashboardString(value string) string {
	clean := strings.ReplaceAll(value, "WAFFLE_AGE_IDENTITY", "[redacted]")
	for _, pattern := range dashboardSensitivePatterns {
		clean = pattern.ReplaceAllString(clean, "[redacted]")
	}
	return clean
}

func safeChatState(state chat.State) chat.State {
	state.Title = sanitizeDashboardString(state.Title)
	state.ModelAlias = sanitizeDashboardString(state.ModelAlias)
	state.ModelError = sanitizeDashboardString(state.ModelError)
	state.ProviderLabel = sanitizeDashboardString(state.ProviderLabel)
	state.Profile = sanitizeDashboardString(state.Profile)
	state.Workspace = sanitizeDashboardString(state.Workspace)
	for i := range state.Models {
		state.Models[i].Alias = sanitizeDashboardString(state.Models[i].Alias)
		state.Models[i].Provider = sanitizeDashboardString(state.Models[i].Provider)
		state.Models[i].Upstream = sanitizeDashboardString(state.Models[i].Upstream)
	}
	return state
}

func safeDashboardChatState(state chat.State) dashboardChatState {
	state = safeChatState(state)
	return dashboardChatState{
		SessionID:      state.SessionID,
		Title:          state.Title,
		ModelAlias:     state.ModelAlias,
		ModelError:     state.ModelError,
		ProviderLabel:  state.ProviderLabel,
		Profile:        state.Profile,
		ConnectionMode: state.ConnectionMode,
		SandboxMode:    state.SandboxMode,
		Workspace:      state.Workspace,
		Models:         state.Models,
		Capabilities:   state.Capabilities,
	}
}

func safeChatResult(result chat.Result) chat.Result {
	result.Title = sanitizeDashboardString(result.Title)
	result.Text = sanitizeDashboardString(result.Text)
	for i := range result.Models {
		result.Models[i].Alias = sanitizeDashboardString(result.Models[i].Alias)
		result.Models[i].Provider = sanitizeDashboardString(result.Models[i].Provider)
		result.Models[i].Upstream = sanitizeDashboardString(result.Models[i].Upstream)
	}
	for i := range result.Sessions {
		result.Sessions[i].Title = sanitizeDashboardString(result.Sessions[i].Title)
		result.Sessions[i].Summary = sanitizeDashboardString(result.Sessions[i].Summary)
		result.Sessions[i].ModelAlias = sanitizeDashboardString(result.Sessions[i].ModelAlias)
	}
	for i := range result.Workset {
		result.Workset[i].Text = sanitizeDashboardString(result.Workset[i].Text)
	}
	if result.State != nil {
		state := safeChatState(*result.State)
		result.State = &state
	}
	return result
}

func detachedTimeoutContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(timeout)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	return context.WithDeadline(context.WithoutCancel(ctx), deadline)
}

func closeBackend(ctx context.Context, backend chat.Backend) {
	closeCtx, cancel := detachedTimeoutContext(ctx, 5*time.Second)
	defer cancel()
	_ = backend.Close(closeCtx)
}
