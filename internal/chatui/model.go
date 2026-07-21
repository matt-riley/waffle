// Package chatui implements Waffle's interactive Focused Conversation terminal UI.
package chatui

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/llm"
)

const (
	defaultWidth  = 100
	defaultHeight = 30
	closeTimeout  = 10 * time.Second

	// Throttle full transcript rebuilds while streaming text tokens.
	// Sync when either at least viewportSyncTokenInterval deltas arrived or
	// viewportSyncMinInterval has elapsed since the last full sync.
	viewportSyncTokenInterval = 12
	viewportSyncMinInterval   = 16 * time.Millisecond
)

// Options controls presentation without exposing backend configuration.
type Options struct {
	Width, Height int
	NoColor       bool
	Context       context.Context
}

type cardRole uint8

const (
	roleUser cardRole = iota
	roleAssistant
	roleNotice
	roleError
)

type messageCard struct {
	role cardRole
	text string
}

type toolRow struct {
	name         string
	messageIndex int
	done         bool
	failed       bool
	byteCount    int
}

type overlayKind uint8

const (
	overlayNone overlayKind = iota
	overlayHelp
	overlayModels
	overlaySessions
	overlayPermissions
	overlayConfirm
)

type overlayItem struct {
	title  string
	detail string
	value  string
}

func (i overlayItem) Title() string       { return i.title }
func (i overlayItem) Description() string { return i.detail }
func (i overlayItem) FilterValue() string { return i.title }

// Model owns all interactive chat and component state.
type Model struct {
	backend chat.Backend
	open    chat.OpenOptions
	ctx     context.Context
	state   chat.State

	viewport               viewport.Model
	composer               textarea.Model
	overlayList            list.Model
	messages               []messageCard
	tools                  []toolRow
	palette                []chat.Command
	overlay                overlayKind
	overlayResult          chat.Result
	pendingConfirm         chat.ParsedCommand
	deferredCommand        *chat.ParsedCommand
	queuedUserInput        string // plain-text submit while turnActive; replace-on-requeue
	stateChangeActive      bool
	commandActive          bool
	commandCancelRequested bool
	commandCancel          context.CancelFunc
	confirmNeedsTurnDrain  bool

	paletteVisible bool
	paletteIndex   int
	turnActive     bool
	quitting       bool
	exitArmed      bool
	connected      bool
	awaitingAck    bool
	opened         bool
	openResolved   bool
	width          int
	height         int
	err            error
	inputTokens    int
	outputTokens   int
	// live*Tokens are absolute mid-turn usage observations for the busy footer.
	// They do not affect the cumulative idle inputTokens/outputTokens totals.
	liveInputTokens  int
	liveOutputTokens int
	turnStartedAt    time.Time
	// now is injectable for tests; defaults to time.Now.
	now              func() time.Time
	theme            theme
	events           chan tea.Msg
	pumpStop         chan struct{}
	pumpStopOnce     *sync.Once
	closed           *atomic.Bool
	nextOperationID  uint64
	activeTurnID     uint64
	canceledTurnID   uint64
	turnTerminalSeen bool

	// Streaming viewport throttle state.
	textDeltaSinceSync int
	lastViewportSync   time.Time
	// syncViewportCalls is a test-only counter of full transcript rebuilds.
	syncViewportCalls int

	// Prompt history for Up/Down traversal (oldest first).
	// historyIdx == -1 means draft mode (not browsing).
	history      []string
	historyIdx   int // index into history; -1 = editing draft
	historyDraft string
}

// New constructs a Focused Conversation model. Backend.Open remains asynchronous
// and is started by Init so model construction never performs I/O.
func New(backend chat.Backend, open chat.OpenOptions, options Options) *Model {
	width, height := options.Width, options.Height
	if width <= 0 {
		width = defaultWidth
	}
	if height <= 0 {
		height = defaultHeight
	}
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}

	composer := textarea.New()
	composer.Placeholder = "Ask Waffle…"
	composer.Prompt = ""
	composer.ShowLineNumbers = false
	composer.DynamicHeight = true
	composer.MinHeight = 1
	composer.MaxHeight = 5
	composer.MaxContentHeight = 12
	composer.CharLimit = 1 << 20
	composer.SetVirtualCursor(true)
	_ = composer.Focus()

	vp := viewport.New()
	vp.SoftWrap = true
	vp.FillHeight = false

	noColor := options.NoColor || os.Getenv("NO_COLOR") != ""
	overlayList := newOverlayList(nil, min(width-12, 76), min(height-10, 16), true, noColor)

	if noColor {
		composer.SetStyles(textarea.Styles{})
		composer.SetVirtualCursor(false)
	}
	m := &Model{
		backend: backend, open: open, ctx: ctx,
		viewport: vp, composer: composer, overlayList: overlayList,
		width: width, height: height, theme: newTheme(true, noColor),
		now:        time.Now,
		events:     make(chan tea.Msg, 64), closed: &atomic.Bool{},
		pumpStop:   make(chan struct{}), pumpStopOnce: &sync.Once{},
		historyIdx: -1,
	}
	m.resize(width, height)
	return m
}

func newOverlayList(items []list.Item, width, height int, dark, noColor bool) list.Model {
	delegate := list.NewDefaultDelegate()
	palette := newTheme(dark, noColor)
	if noColor {
		delegate.Styles = list.DefaultItemStyles{}
	} else {
		delegate.Styles = list.NewDefaultItemStyles(dark)
		delegate.Styles.NormalDesc = delegate.Styles.NormalDesc.Foreground(palette.muted)
		delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.Foreground(palette.brand).BorderForeground(palette.brand)
		delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.Foreground(palette.brand).BorderForeground(palette.brand)
		delegate.Styles.FilterMatch = delegate.Styles.FilterMatch.Foreground(palette.brand)
	}
	model := list.New(items, delegate, width, height)
	if noColor {
		model.Styles = list.Styles{}
	} else {
		model.Styles = list.DefaultStyles(dark)
		model.Styles.Filter.Cursor.Color = palette.brand
		model.Styles.Filter.Focused.Prompt = model.Styles.Filter.Focused.Prompt.Foreground(palette.brand)
		model.Styles.Filter.Blurred.Prompt = model.Styles.Filter.Blurred.Prompt.Foreground(palette.brand)
		model.Styles.ActivePaginationDot = model.Styles.ActivePaginationDot.Foreground(palette.brand)
		model.Styles.DefaultFilterCharacterMatch = model.Styles.DefaultFilterCharacterMatch.Foreground(palette.brand)
	}
	model.SetShowTitle(false)
	model.SetShowFilter(true)
	model.SetShowHelp(false)
	model.SetShowStatusBar(false)
	model.SetShowPagination(true)
	model.SetFilteringEnabled(true)
	return model
}

// Init opens the backend and asks Bubble Tea for terminal background color.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.openCmd(), tea.RequestBackgroundColor, m.waitEventCmd())
}

type openMsg struct {
	state chat.State
	err   error
}

type eventMsg struct {
	operationID uint64
	event       chat.Event
}
type turnDoneMsg struct {
	operationID uint64
	err         error
}
type commandResultMsg struct {
	operationID       uint64
	startedDuringTurn bool
	command           chat.ParsedCommand
	result            chat.Result
	err               error
}
type closeDoneMsg struct{ err error }
type pumpStoppedMsg struct{}

func (m *Model) openCmd() tea.Cmd {
	return func() tea.Msg {
		state, err := m.backend.Open(m.ctx, m.open)
		return openMsg{state: state, err: err}
	}
}

func (m *Model) nextOperation() uint64 {
	m.nextOperationID++
	return m.nextOperationID
}

func (m *Model) turnCmd(operationID uint64, input string) tea.Cmd {
	return func() tea.Msg {
		err := m.backend.Turn(m.ctx, input, func(event chat.Event) {
			m.sendOperation(eventMsg{operationID: operationID, event: event})
		})
		m.sendOperation(turnDoneMsg{operationID: operationID, err: err})
		return nil
	}
}

func (m *Model) commandCmd(ctx context.Context, operationID uint64, command chat.ParsedCommand, startedDuringTurn bool) tea.Cmd {
	return func() tea.Msg {
		result, err := m.backend.Command(ctx, command, func(event chat.Event) {
			m.sendOperation(eventMsg{operationID: operationID, event: event})
		})
		m.sendOperation(commandResultMsg{operationID: operationID, startedDuringTurn: startedDuringTurn, command: command, result: result, err: err})
		return nil
	}
}

func (m *Model) waitEventCmd() tea.Cmd {
	return func() tea.Msg {
		select {
		case message := <-m.events:
			return message
		case <-m.pumpStop:
			return pumpStoppedMsg{}
		case <-m.ctx.Done():
			return pumpStoppedMsg{}
		}
	}
}

func (m *Model) sendOperation(message tea.Msg) {
	select {
	case m.events <- message:
	case <-m.pumpStop:
	case <-m.ctx.Done():
	}
}

func (m *Model) closeCmd() tea.Cmd {
	return func() tea.Msg {
		m.pumpStopOnce.Do(func() { close(m.pumpStop) })
		if !m.closed.CompareAndSwap(false, true) {
			return closeDoneMsg{}
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), closeTimeout)
		defer cancel()
		return closeDoneMsg{err: m.backend.Close(ctx)}
	}
}

func cardsFromHistory(history []llm.Message) []messageCard {
	cards := make([]messageCard, 0, len(history))
	for _, msg := range history {
		text := strings.TrimSpace(msg.Text())
		if text == "" {
			continue
		}
		role := roleAssistant
		if msg.Role == llm.RoleUser {
			role = roleUser
		}
		cards = append(cards, messageCard{role: role, text: text})
	}
	return cards
}
