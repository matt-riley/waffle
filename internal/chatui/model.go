// Package chatui implements Waffle's interactive Focused Conversation terminal UI.
package chatui

import (
	"context"
	"os"
	"strings"
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

	viewport       viewport.Model
	composer       textarea.Model
	overlayList    list.Model
	messages       []messageCard
	tools          []toolRow
	palette        []chat.Command
	overlay        overlayKind
	overlayResult  chat.Result
	pendingConfirm chat.ParsedCommand

	paletteVisible bool
	paletteIndex   int
	turnActive     bool
	quitting       bool
	exitArmed      bool
	connected      bool
	awaitingAck    bool
	opened         bool
	width          int
	height         int
	err            error
	inputTokens    int
	outputTokens   int
	theme          theme
	events         chan tea.Msg
	closed         *atomic.Bool
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
		events: make(chan tea.Msg, 64), closed: &atomic.Bool{},
	}
	m.resize(width, height)
	return m
}

func newOverlayList(items []list.Item, width, height int, dark, noColor bool) list.Model {
	delegate := list.NewDefaultDelegate()
	if noColor {
		delegate.Styles = list.DefaultItemStyles{}
	} else {
		delegate.Styles = list.NewDefaultItemStyles(dark)
	}
	model := list.New(items, delegate, width, height)
	if noColor {
		model.Styles = list.Styles{}
	} else {
		model.Styles = list.DefaultStyles(dark)
	}
	model.SetShowTitle(false)
	model.SetShowFilter(false)
	model.SetShowHelp(false)
	model.SetShowStatusBar(false)
	model.SetShowPagination(false)
	model.SetFilteringEnabled(false)
	return model
}

// Init opens the backend and asks Bubble Tea for terminal background color.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.openCmd(), tea.RequestBackgroundColor)
}

type openMsg struct {
	state chat.State
	err   error
}

type eventMsg struct{ event chat.Event }
type turnDoneMsg struct{ err error }
type commandResultMsg struct {
	command chat.ParsedCommand
	result  chat.Result
	err     error
}
type closeDoneMsg struct{ err error }

func (m *Model) openCmd() tea.Cmd {
	return func() tea.Msg {
		state, err := m.backend.Open(m.ctx, m.open)
		return openMsg{state: state, err: err}
	}
}

func (m *Model) turnCmd(input string) tea.Cmd {
	return func() tea.Msg {
		err := m.backend.Turn(m.ctx, input, func(event chat.Event) { m.events <- eventMsg{event: event} })
		m.events <- turnDoneMsg{err: err}
		return nil
	}
}

func (m *Model) commandCmd(command chat.ParsedCommand) tea.Cmd {
	return func() tea.Msg {
		result, err := m.backend.Command(m.ctx, command, func(event chat.Event) { m.events <- eventMsg{event: event} })
		m.events <- commandResultMsg{command: command, result: result, err: err}
		return nil
	}
}

func (m *Model) waitEventCmd() tea.Cmd {
	return func() tea.Msg { return <-m.events }
}

func (m *Model) closeCmd() tea.Cmd {
	return func() tea.Msg {
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
