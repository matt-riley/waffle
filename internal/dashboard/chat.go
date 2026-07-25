package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/llm"
)

const (
	defaultChatClientLimit = 64
	defaultChatIdleTTL     = 30 * time.Minute
	// defaultChatReattachRecoveryTTL bounds the previous proof kept solely for
	// retrying a reattach response that was lost in transit.
	defaultChatReattachRecoveryTTL = 30 * time.Second
	chatIDAttempts                 = 8
)

var (
	errChatClientNotFound = errors.New("chat_client_not_found")
	errChatTurnActive     = errors.New("turn_active")
	errChatUnavailable    = errors.New("chat_unavailable")
)

// BackendFactory builds one isolated chat backend for each browser client.
type BackendFactory func(context.Context) (chat.Backend, error)

type chatClient struct {
	backend         chat.Backend
	state           chat.State
	reattachToken   string
	previousToken   string
	previousExpires time.Time
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
	retiring        bool
}

// ChatClientLease is the server-issued proof required to reattach to an
// existing browser chat owner. A client ID without its proof is only an
// identifier and never authorizes reattachment.
type ChatClientLease struct {
	ClientID      string
	ReattachToken string
}

func (c *chatClient) prepareCancel() (chan struct{}, bool) {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	if c.closed || c.closing || c.retiring {
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
			c.lifecycle.Lock()
			err := c.closeErr
			c.lifecycle.Unlock()
			return err
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
				return c.close(ctx)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	c.retiring = true
	c.closing = true
	c.closeDone = make(chan struct{})
	done := c.closeDone
	c.lifecycle.Unlock()

	err := c.backend.Close(ctx)
	c.lifecycle.Lock()
	c.closeErr = err
	c.closed = cleanupCompleted(err)
	c.closing = false
	close(done)
	c.lifecycle.Unlock()
	return err
}

func (c *chatClient) markRetiring() {
	c.lifecycle.Lock()
	c.retiring = true
	c.lifecycle.Unlock()
}

func (c *chatClient) isRetiring() bool {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	return c.retiring
}

func (c *chatClient) cleanupCompleted() bool {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	return c.closed
}

func cleanupCompleted(err error) bool {
	if err == nil {
		return true
	}
	var completed interface{ CleanupCompleted() bool }
	return errors.As(err, &completed) && completed.CleanupCompleted()
}

func (c *chatClient) beginOperation() bool {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	if c.closed || c.closing || c.retiring {
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
	mu          sync.Mutex
	clients     map[string]*chatClient
	factory     BackendFactory
	ids         io.Reader
	now         func() time.Time
	maxClients  int
	idleTTL     time.Duration
	shutdownTTL time.Duration
	events      *EventHub
	// redact replaces known secret values with placeholders. Production wiring
	// supplies secret.Redactor.Redact via SetRedactor so free-form chat text
	// never relies on format-guessing regexes at the Desk boundary (#153).
	// Unexported so concurrent event reads always go through redactExact under
	// the mutex rather than a racy direct field write.
	redact           func(string) string
	shutting         bool
	shutdownDone     chan struct{}
	shutdownErr      error
	shutdownRunning  bool
	shutdownComplete bool
	pending          map[*chatClient]*chatCleanup
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

// SetRedactor installs the exact-value secret redactor used on chat projections.
// The sole write path for the redactor; callers must not assign a field.
func (c *ChatClients) SetRedactor(redact func(string) string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.redact = redact
}

// Open creates and opens one isolated backend for a browser client.
func (c *ChatClients) Open(ctx context.Context, options chat.OpenOptions) (string, chat.State, error) {
	lease, state, err := c.OpenWithLease(ctx, options, ChatClientLease{})
	return lease.ClientID, state, err
}

// OpenWithLease opens a new backend or reattaches to the exact existing
// browser owner proven by prior. Successful reattachment rotates the current
// proof while retaining the previous proof for one lost-response recovery
// within a short bounded window.
func (c *ChatClients) OpenWithLease(
	ctx context.Context,
	options chat.OpenOptions,
	prior ChatClientLease,
) (ChatClientLease, chat.State, error) {
	if err := c.reap(ctx); err != nil {
		return ChatClientLease{}, chat.State{}, err
	}
	if prior.ClientID != "" || prior.ReattachToken != "" {
		return c.reattach(prior)
	}
	c.mu.Lock()
	if c.shutting || c.factory == nil || len(c.clients) >= c.maxClients {
		c.mu.Unlock()
		return ChatClientLease{}, chat.State{}, errChatUnavailable
	}
	c.mu.Unlock()

	backend, err := c.factory(ctx)
	if err != nil || backend == nil {
		return ChatClientLease{}, chat.State{}, errChatUnavailable
	}
	state, err := backend.Open(ctx, options)
	if err != nil {
		closeBackend(ctx, backend)
		return ChatClientLease{}, chat.State{}, err
	}
	reattachToken, err := newReattachToken()
	if err != nil {
		closeBackend(ctx, backend)
		return ChatClientLease{}, chat.State{}, errChatUnavailable
	}
	c.mu.Lock()
	if c.shutting || len(c.clients) >= c.maxClients {
		c.mu.Unlock()
		closeBackend(ctx, backend)
		return ChatClientLease{}, chat.State{}, errChatUnavailable
	}
	var clientID string
	for attempts := 0; attempts < chatIDAttempts; attempts++ {
		candidate, idErr := c.newClientID()
		if idErr != nil {
			c.mu.Unlock()
			closeBackend(ctx, backend)
			return ChatClientLease{}, chat.State{}, errChatUnavailable
		}
		if _, exists := c.clients[candidate]; !exists {
			clientID = candidate
			break
		}
	}
	if clientID == "" {
		c.mu.Unlock()
		closeBackend(ctx, backend)
		return ChatClientLease{}, chat.State{}, errChatUnavailable
	}
	c.clients[clientID] = &chatClient{
		backend:       backend,
		state:         cloneChatState(state),
		reattachToken: reattachToken,
		lastActive:    c.now(),
	}
	c.mu.Unlock()
	return ChatClientLease{ClientID: clientID, ReattachToken: reattachToken}, state, nil
}

func (c *ChatClients) reattach(prior ChatClientLease) (ChatClientLease, chat.State, error) {
	if prior.ClientID == "" || prior.ReattachToken == "" {
		return ChatClientLease{}, chat.State{}, errChatClientNotFound
	}
	nextToken, err := newReattachToken()
	if err != nil {
		return ChatClientLease{}, chat.State{}, errChatUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	client, ok := c.clients[prior.ClientID]
	if !ok || c.shutting || client.isRetiring() {
		return ChatClientLease{}, chat.State{}, errChatClientNotFound
	}
	now := c.now()
	currentProof := sameReattachToken(client.reattachToken, prior.ReattachToken)
	previousProof := sameReattachToken(client.previousToken, prior.ReattachToken) &&
		now.Before(client.previousExpires)
	if !currentProof && !previousProof {
		return ChatClientLease{}, chat.State{}, errChatClientNotFound
	}
	client.previousToken = client.reattachToken
	client.previousExpires = now.Add(defaultChatReattachRecoveryTTL)
	client.reattachToken = nextToken
	client.lastActive = now
	return ChatClientLease{
		ClientID:      prior.ClientID,
		ReattachToken: nextToken,
	}, cloneChatState(client.state), nil
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
	result, err := client.backend.Command(operationCtx, command, c.emit(clientID))
	if err == nil && result.State != nil {
		c.rememberState(clientID, client, *result.State)
	}
	return result, err
}

// Cancel cancels a client backend without holding the manager lock.
func (c *ChatClients) Cancel(clientID string) error {
	c.mu.Lock()
	client, ok := c.clients[clientID]
	if ok && client.isRetiring() {
		ok = false
	}
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

// Close retires a client and removes it only after its backend closes
// successfully. A failed close keeps the same ID and backend available for an
// explicit cleanup retry while rejecting further client operations.
func (c *ChatClients) Close(ctx context.Context, clientID string) error {
	return c.closeClient(ctx, ChatClientLease{ClientID: clientID}, false)
}

// CloseWithLease closes only when lease still proves the current owner. The
// previous proof accepted for lost reattach responses is deliberately rejected
// here so a stale pagehide callback cannot close a newly reattached page.
func (c *ChatClients) CloseWithLease(ctx context.Context, lease ChatClientLease) error {
	return c.closeClient(ctx, lease, true)
}

func (c *ChatClients) closeClient(ctx context.Context, lease ChatClientLease, verifyLease bool) error {
	c.mu.Lock()
	client, ok := c.clients[lease.ClientID]
	if ok && verifyLease && !sameReattachToken(client.reattachToken, lease.ReattachToken) {
		ok = false
	}
	var done <-chan struct{}
	var operationCancel context.CancelFunc
	if ok {
		client.markRetiring()
		if client.busy {
			done = client.done
			operationCancel = client.operationCancel
		}
	}
	if !ok {
		c.mu.Unlock()
		return errChatClientNotFound
	}
	cleanup := c.startCleanupLocked(lease.ClientID, client, done, operationCancel, ctx)
	c.mu.Unlock()
	select {
	case <-cleanup.done:
		return cleanup.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *ChatClients) rememberState(clientID string, client *chatClient, state chat.State) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clients[clientID] == client {
		client.state = cloneChatState(state)
	}
}

// Shutdown stops future opens and closes all live backends exactly once.
func (c *ChatClients) Shutdown(ctx context.Context) error {
	closeCtx, closeCancel := detachedTimeoutContext(ctx, c.shutdownTTL)
	defer closeCancel()
	c.mu.Lock()
	if c.shutdownRunning {
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
	if c.shutdownComplete {
		err := c.shutdownErr
		c.mu.Unlock()
		return err
	}
	c.shutting = true
	c.shutdownRunning = true
	c.shutdownDone = make(chan struct{})
	cleanups := make([]*chatCleanup, 0, len(c.clients))
	for clientID, client := range c.clients {
		client.markRetiring()
		var done <-chan struct{}
		var operationCancel context.CancelFunc
		if client.busy {
			done = client.done
			operationCancel = client.operationCancel
		}
		cleanups = append(cleanups, c.startCleanupLocked(clientID, client, done, operationCancel, closeCtx))
	}
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
	c.shutdownRunning = false
	c.shutdownComplete = len(c.clients) == 0 && len(c.pending) == 0
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
	if client.isRetiring() {
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
		if !client.busy && !c.now().Before(idleDeadline(client.lastActive, c.idleTTL)) {
			client.markRetiring()
			cleanups = append(cleanups, c.startCleanupLocked(id, client, nil, nil, ctx))
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
	clientID string,
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
		if client.cleanupCompleted() && c.clients[clientID] == client {
			delete(c.clients, clientID)
		}
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

func newReattachToken() (string, error) {
	bytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, bytes); err != nil {
		return "", fmt.Errorf("read reattach token: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

func sameReattachToken(left, right string) bool {
	if len(left) != len(right) || left == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func cloneChatState(state chat.State) chat.State {
	cloned := state
	cloned.History = append([]llm.Message(nil), state.History...)
	for i := range cloned.History {
		cloned.History[i].Blocks = append([]llm.Block(nil), state.History[i].Blocks...)
	}
	cloned.Models = append([]chat.Model(nil), state.Models...)
	cloned.Skills = append([]chat.SkillRef(nil), state.Skills...)
	cloned.Capabilities = append([]string(nil), state.Capabilities...)
	return cloned
}

func (c *ChatClients) emit(clientID string) func(chat.Event) {
	return func(event chat.Event) {
		if event.State != nil {
			c.mu.Lock()
			if client := c.clients[clientID]; client != nil {
				client.state = cloneChatState(*event.State)
				client.lastActive = c.now()
			}
			c.mu.Unlock()
		}
		if c.events == nil {
			return
		}
		if !dashboardEventKind(event.Kind) {
			return
		}
		// Structural guarantees (#153): error events never carry free-form
		// provider text; tool names must be identifiers; free-form text is
		// exact-value redacted rather than pattern-guessed.
		text := c.projectChatText(event)
		var state *dashboardChatState
		if event.State != nil {
			safe := c.safeDashboardChatState(*event.State)
			state = &safe
		}
		data, err := json.Marshal(dashboardChatEvent{
			Kind:       event.Kind,
			Text:       text,
			ToolName:   projectChatToolName(event.ToolName),
			ToolCallID: projectChatToolCallID(event.ToolCallID),
			IsError:    event.IsError,
			ByteCount:  event.ByteCount,
			DurationMS: event.DurationMS,
			State:      state,
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

func (c *ChatClients) projectChatText(event chat.Event) string {
	if event.IsError {
		return "chat operation failed"
	}
	return c.redactExact(event.Text)
}

func (c *ChatClients) redactExact(value string) string {
	if value == "" {
		return value
	}
	var redact func(string) string
	if c != nil {
		c.mu.Lock()
		redact = c.redact
		c.mu.Unlock()
	}
	if redact != nil {
		value = redact(value)
	}
	// Identity env name is configuration surface, not a secret-format guess.
	return strings.ReplaceAll(value, "WAFFLE_AGE_IDENTITY", "[redacted]")
}

// projectChatToolName admits tool identifiers only. Paths and free-form
// material never qualify, so host paths cannot arrive as tool names.
func projectChatToolName(name string) string {
	return projectChatIdentifier(name)
}

func projectChatToolCallID(id string) string {
	return projectChatIdentifier(id)
}

func projectChatIdentifier(name string) string {
	if name == "" {
		return ""
	}
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' {
			continue
		}
		return "[redacted]"
	}
	return name
}

// projectChatWorkspaceLabel drops absolute host paths so waffle data roots
// never appear in browser-facing state.
func projectChatWorkspaceLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "/") || strings.ContainsAny(value, `:\`) {
		return ""
	}
	return value
}

type dashboardChatEvent struct {
	Kind       chat.EventKind      `json:"kind"`
	Text       string              `json:"text,omitempty"`
	ToolName   string              `json:"tool_name,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
	IsError    bool                `json:"is_error,omitempty"`
	ByteCount  int                 `json:"byte_count,omitempty"`
	DurationMS int64               `json:"duration_ms,omitempty"`
	Usage      dashboardUsage      `json:"usage,omitempty"`
	State      *dashboardChatState `json:"state,omitempty"`
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

func (c *ChatClients) safeChatState(state chat.State) chat.State {
	state.Title = c.redactExact(state.Title)
	state.ModelAlias = c.redactExact(state.ModelAlias)
	state.ModelError = c.redactExact(state.ModelError)
	state.ProviderLabel = c.redactExact(state.ProviderLabel)
	state.Profile = c.redactExact(state.Profile)
	state.Workspace = projectChatWorkspaceLabel(c.redactExact(state.Workspace))
	for i := range state.Models {
		state.Models[i].Alias = c.redactExact(state.Models[i].Alias)
		state.Models[i].Provider = c.redactExact(state.Models[i].Provider)
		state.Models[i].Upstream = c.redactExact(state.Models[i].Upstream)
	}
	return state
}

func (c *ChatClients) safeDashboardChatState(state chat.State) dashboardChatState {
	state = c.safeChatState(state)
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

func (c *ChatClients) safeChatResult(result chat.Result) chat.Result {
	result.Title = c.redactExact(result.Title)
	result.Text = c.redactExact(result.Text)
	for i := range result.Models {
		result.Models[i].Alias = c.redactExact(result.Models[i].Alias)
		result.Models[i].Provider = c.redactExact(result.Models[i].Provider)
		result.Models[i].Upstream = c.redactExact(result.Models[i].Upstream)
	}
	for i := range result.Sessions {
		result.Sessions[i].Title = c.redactExact(result.Sessions[i].Title)
		result.Sessions[i].Summary = c.redactExact(result.Sessions[i].Summary)
		result.Sessions[i].ModelAlias = c.redactExact(result.Sessions[i].ModelAlias)
	}
	for i := range result.Workset {
		result.Workset[i].Text = c.redactExact(result.Workset[i].Text)
	}
	if result.Permissions != nil {
		result.Permissions.SandboxMode = c.redactExact(result.Permissions.SandboxMode)
		result.Permissions.Allow = c.projectChatPermissions(result.Permissions.Allow)
		result.Permissions.Deny = c.projectChatPermissions(result.Permissions.Deny)
		result.Permissions.DenyPrefixes = c.projectChatPermissions(result.Permissions.DenyPrefixes)
	}
	if result.State != nil {
		state := c.safeChatState(*result.State)
		result.State = &state
	}
	return result
}

func (c *ChatClients) projectChatPermissions(values []string) []string {
	projected := make([]string, len(values))
	for i, value := range values {
		redacted := c.redactExact(value)
		if redacted != value {
			projected[i] = redacted
			continue
		}
		if !chatPermissionIdentifier(value) {
			projected[i] = "[redacted]"
			continue
		}
		projected[i] = value
	}
	return projected
}

func chatPermissionIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) ||
			r == '_' || r == '-' || r == '.' || r == '*' {
			continue
		}
		return false
	}
	return true
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
