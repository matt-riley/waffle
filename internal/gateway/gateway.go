// Package gateway is waffle's control plane (docs/plan.md): it owns the
// channel adapters, routes every inbound message through the entity model,
// and runs the agent for recognized conversations. waffle is single-owner:
// messages from anyone but the owner earn a pairing code and nothing else.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/channel"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/entity"
	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/usage"
)

// Gateway wires adapters to the agent runtime.
type Gateway struct {
	// Agent remains the main group fallback for existing callers.
	Agent  *agent.Agent
	Agents map[string]*agent.Agent
	// Profiles maps named agent profiles built against the main trust tier (#71).
	// When a channel group binds a Profile and this map is non-nil, the
	// matching agent is used; an unknown profile errors rather than falling
	// back to the group agent. When the map is nil, Profile is ignored
	// (tests and partial wiring).
	Profiles map[string]*agent.Agent
	// GroupProfiles maps profiles built against the multiparty "group" tier
	// so a bind cannot widen tools past group policy (#71 / #34).
	GroupProfiles map[string]*agent.Agent
	Entities      *entity.Store
	Sessions      *session.Store
	Adapters      []channel.Adapter
	Log           *slog.Logger

	// Observability records gateway agent runs when configured.
	Observability *observability.Service
	Usage         *usage.Store

	// MaxConcurrent bounds in-flight message handlers so a flooding
	// channel can't spawn unbounded goroutines. Zero means the default.
	MaxConcurrent int
	// ReflectEveryTurns, when > 0, writes a session summary every N turns (#59).
	ReflectEveryTurns int
	// DrainTimeout bounds shutdown waiting for accepted handlers. The timeout
	// starts only after shutdown begins; zero uses DefaultDrainTimeout.
	DrainTimeout time.Duration
	// PostCancelGrace bounds the wait for context-aware handler cleanup after
	// DrainTimeout cancels accepted work. A truly non-cooperative handler is
	// then awaited to keep shared resources alive rather than closing under it.
	PostCancelGrace time.Duration

	mu     sync.Mutex
	groups map[string]*groupLock // active per-conversation serialization
}

func (g *Gateway) agentFor(group string) (*agent.Agent, error) {
	if g.Agents != nil {
		if selected := g.Agents[group]; selected != nil {
			return selected, nil
		}
	}
	if group == "main" && g.Agent != nil {
		return g.Agent, nil
	}
	return nil, fmt.Errorf("gateway: no agent configured for group %s", group)
}

// agentForGroup resolves the agent for a channel group, applying a named
// profile bind when set (#71). Multiparty groups use GroupProfiles so the
// bound profile cannot widen tools past the restricted group tier (#34).
func (g *Gateway) agentForGroup(group *entity.Group) (*agent.Agent, error) {
	if group == nil {
		return nil, fmt.Errorf("gateway: nil group")
	}
	selected, err := g.agentFor(group.AgentGroup)
	if err != nil {
		return nil, err
	}
	if group.Profile == "" {
		return selected, nil
	}
	// Prefer tier-matched profile maps.
	var tier map[string]*agent.Agent
	switch group.AgentGroup {
	case config.GroupGroup:
		tier = g.GroupProfiles
		if tier == nil {
			// Fall back to main-tier profiles only when group map is unset
			// (tests); production always wires GroupProfiles.
			tier = g.Profiles
		}
	default:
		tier = g.Profiles
	}
	if tier == nil {
		return selected, nil
	}
	if p := tier[group.Profile]; p != nil {
		return p, nil
	}
	return nil, fmt.Errorf("gateway: unknown profile %q", group.Profile)
}

type groupLock struct {
	mu   sync.Mutex
	refs int
}

// defaultMaxConcurrent caps simultaneous message handlers. Generous for a
// single-owner agent, but bounded so a misbehaving channel can't exhaust
// memory.
const defaultMaxConcurrent = 8

// DefaultDrainTimeout gives accepted work a useful grace period while
// ensuring a wedged provider or tool cannot retain gateway ownership forever.
const DefaultDrainTimeout = 30 * time.Second
const DefaultPostCancelGrace = 5 * time.Second

// Run starts every adapter and processes inbound messages until ctx ends.
func (g *Gateway) Run(ctx context.Context) error {
	if g.Log == nil {
		g.Log = slog.Default()
	}
	g.groups = make(map[string]*groupLock)

	inbound := make(chan channel.Message, 64)
	var adapters sync.WaitGroup
	for _, a := range g.Adapters {
		adapters.Add(1)
		go func(a channel.Adapter) {
			defer adapters.Done()
			if err := a.Run(ctx, inbound); err != nil {
				g.Log.Error("adapter stopped", "channel", a.Name(), "err", err)
			}
		}(a)
		g.Log.Info("channel up", "channel", a.Name())
	}

	// Bound in-flight handlers with a semaphore, and track them so shutdown
	// drains work already accepted rather than abandoning it mid-turn.
	maxConcurrent := g.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrent
	}
	sem := make(chan struct{}, maxConcurrent)
	var handlers sync.WaitGroup
	drainCtx, cancelDrain := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelDrain()

	for {
		select {
		case <-ctx.Done():
			adaptersDone := make(chan struct{})
			go func() {
				adapters.Wait()
				close(adaptersDone)
			}()
			handlersDone := make(chan struct{})
			go func() {
				handlers.Wait()
				close(handlersDone)
			}()
			timeout := g.DrainTimeout
			if timeout <= 0 {
				timeout = DefaultDrainTimeout
			}
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			adaptersFinished, handlersFinished := false, false
			for !adaptersFinished || !handlersFinished {
				select {
				case <-adaptersDone:
					adaptersFinished = true
					adaptersDone = nil
				case <-handlersDone:
					handlersFinished = true
					handlersDone = nil
				case <-timer.C:
					cancelDrain()
					if !adaptersFinished {
						g.Log.Warn("gateway adapter drain timed out", "timeout", timeout)
					}
					if handlersFinished {
						return nil
					}
					grace := g.PostCancelGrace
					if grace <= 0 {
						grace = DefaultPostCancelGrace
					}
					graceTimer := time.NewTimer(grace)
					select {
					case <-handlersDone:
						graceTimer.Stop()
						return nil
					case <-graceTimer.C:
						// Go cannot kill a goroutine safely. Keep shared resources
						// alive until a handler that ignored cancellation exits.
						g.Log.Error("gateway handler ignored cancellation; holding resources", "grace", grace)
						<-handlersDone
						return nil
					}
				}
			}
			return nil
		case msg := <-inbound:
			if ctx.Err() != nil {
				continue
			}
			// Acquire a slot before spawning: under a flood this blocks the
			// loop (applying backpressure) instead of piling up goroutines.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				continue
			}
			if ctx.Err() != nil {
				<-sem
				continue
			}
			handlers.Add(1)
			// Detach from the shutdown context so that handlers already
			// accepted can run to completion and persist their turn even
			// after ctx is canceled.  Adapters are stopped by ctx; new
			// messages are rejected above; only the drain path reaches here.
			go func(msg channel.Message) {
				defer handlers.Done()
				defer func() { <-sem }()
				// Handle concurrently across conversations, serially within
				// one (the per-group lock).
				g.handle(drainCtx, msg)
			}(msg)
		}
	}
}

func (g *Gateway) adapter(name string) channel.Adapter {
	for _, a := range g.Adapters {
		if a.Name() == name {
			return a
		}
	}
	return nil
}

func (g *Gateway) ensureGroups() {
	if g.groups == nil {
		g.groups = make(map[string]*groupLock)
	}
}

func (g *Gateway) lockGroup(key string) func() {
	g.mu.Lock()
	g.ensureGroups()
	l, ok := g.groups[key]
	if !ok {
		l = &groupLock{}
		g.groups[key] = l
	}
	l.refs++
	g.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		g.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(g.groups, key)
		}
		g.mu.Unlock()
	}
}

// tryLockGroup acquires the conversation lock without blocking. ok is false
// when another handler already holds it (#59 idle reflection).
func (g *Gateway) tryLockGroup(key string) (unlock func(), ok bool) {
	g.mu.Lock()
	g.ensureGroups()
	l, exists := g.groups[key]
	if !exists {
		l = &groupLock{}
		g.groups[key] = l
	}
	l.refs++
	g.mu.Unlock()

	if !l.mu.TryLock() {
		g.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(g.groups, key)
		}
		g.mu.Unlock()
		return nil, false
	}
	return func() {
		l.mu.Unlock()
		g.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(g.groups, key)
		}
		g.mu.Unlock()
	}, true
}

// TryLockSession locks the channel+chat group for a session when one is
// mapped; sessions without a channel group need no lock. Returns ok=false
// when the conversation is busy with message handling (#59).
func (g *Gateway) TryLockSession(ctx context.Context, sessionID string) (unlock func(), ok bool) {
	if g.Entities == nil || sessionID == "" {
		return func() {}, true
	}
	ch, chatID, found, err := g.Entities.ChannelChatForSession(ctx, sessionID)
	if err != nil || !found {
		// CLI / unbound sessions: no group lock required.
		return func() {}, true
	}
	return g.tryLockGroup(ch + "\x00" + chatID)
}

// ReflectSession writes a session summary under the conversation group lock
// when the session maps to a channel group. Skips when locked (busy) or when
// a summary is already present (at most once per quiet period) (#59).
func (g *Gateway) ReflectSession(ctx context.Context, sessionID string) (wrote bool, err error) {
	if g.Sessions == nil || sessionID == "" {
		return false, nil
	}
	unlock, ok := g.TryLockSession(ctx, sessionID)
	if !ok {
		return false, nil
	}
	defer unlock()
	return g.reflectSessionLocked(ctx, sessionID, nil, "", true)
}

// reflectSessionLocked assumes the caller holds the conversation lock (or none
// is needed). history/provider/model may be provided by the turn path to avoid
// reloading; empty history reloads from the store. When onlyIfEmpty is true,
// an existing summary is left alone (idle / quiet-period path).
func (g *Gateway) reflectSessionLocked(ctx context.Context, sessionID string, history []llm.Message, model string, onlyIfEmpty bool) (bool, error) {
	log := g.Log
	if log == nil {
		log = slog.Default()
	}
	if onlyIfEmpty {
		sess, err := g.Sessions.Get(ctx, sessionID)
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(sess.Summary) != "" {
			return false, nil
		}
	}
	var err error
	if history == nil {
		history, err = g.Sessions.Turns(ctx, sessionID)
		if err != nil {
			return false, err
		}
	}
	if len(history) < 2 {
		return false, nil
	}
	provider, reflectModel := g.providerForSession(ctx, sessionID)
	if model != "" {
		reflectModel = model
	}
	if provider == nil {
		return false, nil
	}
	summary, err := session.Reflect(ctx, provider, history, session.ReflectOptions{Model: reflectModel})
	if err != nil {
		log.Warn("session reflection failed", "session_id", sessionID, "err", err)
		return false, err
	}
	if summary == "" {
		return false, nil
	}
	if err := g.Sessions.SetSummary(ctx, sessionID, summary); err != nil {
		log.Warn("session summary persist failed", "session_id", sessionID, "err", err)
		return false, err
	}
	return true, nil
}

func (g *Gateway) providerForSession(ctx context.Context, sessionID string) (llm.Provider, string) {
	// Prefer the agent bound to the channel group (and profile, if set); fall back to main.
	if g.Entities != nil {
		ch, chatID, found, err := g.Entities.ChannelChatForSession(ctx, sessionID)
		if err == nil && found {
			if group, err := g.Entities.GroupFor(ctx, ch, chatID, ""); err == nil {
				if selected, err := g.agentForGroup(group); err == nil && selected != nil {
					model := selected.Model
					if selected.UtilityModel != "" {
						model = selected.UtilityModel
					}
					return selected.Provider, model
				}
			}
		}
	}
	if g.Agent != nil {
		model := g.Agent.Model
		if g.Agent.UtilityModel != "" {
			model = g.Agent.UtilityModel
		}
		return g.Agent.Provider, model
	}
	return nil, ""
}

func (g *Gateway) handle(ctx context.Context, msg channel.Message) {
	if g.Observability != nil {
		g.Observability.MarkAdapter(msg.Channel)
	}
	log := g.Log.With("channel", msg.Channel, "chat", msg.ChatID)
	adapter := g.adapter(msg.Channel)
	if adapter == nil {
		log.Error("message from unknown adapter")
		return
	}

	// Who is this? Not the owner → pairing code in private chats only.
	// Group chats: silent ignore — never mint or spam pairing codes (#34).
	if _, err := g.Entities.Identify(ctx, msg.Channel, msg.SenderID); err != nil {
		if !errors.Is(err, entity.ErrUnknownSender) {
			log.Error("identify", "err", err)
			return
		}
		if msg.IsGroup {
			log.Info("ignored unknown sender in group", "sender", msg.SenderID)
			return
		}
		pairing, err := g.Entities.Pair(ctx, msg.Channel, msg.SenderID, msg.SenderName, msg.ChatID)
		if err != nil {
			log.Error("pair", "err", err)
			return
		}
		log.Info("pairing request", "sender", msg.SenderID, "code", pairing.Code)
		reply := fmt.Sprintf("waffle here. I only talk to my owner.\nPairing code: %s\nIf this is your waffle, run on its host:\n  waffle pair approve %s", pairing.Code, pairing.Code)
		if err := adapter.Send(ctx, msg.ChatID, reply); err != nil {
			log.Error("send pairing reply", "err", err)
		}
		return
	}

	unlock := g.lockGroup(msg.Channel + "\x00" + msg.ChatID)
	defer unlock()

	reply, err := g.converse(ctx, msg)
	if err != nil {
		log.Error("agent run", "err", err)
		detail := fmt.Sprintf("%v", err)
		if group, groupErr := g.Entities.GroupFor(ctx, msg.Channel, msg.ChatID, agentGroupFor(msg)); groupErr == nil {
			if selected, agentErr := g.agentForGroup(group); agentErr == nil && selected != nil && selected.Redact != nil {
				detail = selected.Redact(detail)
			}
		}
		// Keep short to avoid channel limits and excessive internal detail.
		if len(detail) > 200 {
			detail = detail[:200] + "..."
		}
		reply = "something went wrong: " + detail
	}
	if reply == "" {
		return
	}
	if err := adapter.Send(ctx, msg.ChatID, reply); err != nil {
		log.Error("send reply", "err", err)
	}
}

// agentGroupFor chooses the agent tier for a new channel group. Group chats
// use the restricted "group" tier; private chats use "main". Existing
// channel_groups rows keep their stored binding.
func agentGroupFor(msg channel.Message) string {
	if msg.IsGroup {
		return config.GroupGroup
	}
	return config.GroupMain
}

// converse routes one owner message through the conversation's session.
func (g *Gateway) converse(ctx context.Context, msg channel.Message) (string, error) {
	if g.Usage != nil {
		paused, err := g.Usage.Paused(ctx)
		if err != nil {
			return "", err
		}
		if paused {
			return "", errors.New("waffle is paused")
		}
	}
	group, err := g.Entities.GroupFor(ctx, msg.Channel, msg.ChatID, agentGroupFor(msg))
	if err != nil {
		return "", err
	}
	selected, err := g.agentForGroup(group)
	if err != nil {
		return "", err
	}
	history, err := g.Sessions.Turns(ctx, group.SessionID)
	if err != nil {
		return "", err
	}
	history = session.Repair(history)
	persisted := len(history)

	history = append(history, llm.UserText(msg.Text))
	log := g.Log
	if log == nil {
		log = slog.Default()
	}
	profileName := group.Profile
	if profileName == "" {
		profileName = "main"
	}
	log = log.With("session_id", group.SessionID, "profile", profileName)
	log.Info("gateway run started")
	defer log.Info("gateway run finished")

	var runID string
	if g.Observability != nil {
		var err error
		runID, err = id.New("run-")
		if err != nil {
			log.Error("new observability run id", "err", err)
		} else if err := g.Observability.Start(ctx, runID, group.SessionID, "gateway", "agent", group.Profile); err != nil {
			log.Error("start observability run", "err", err)
			runID = ""
		}
	}
	ctx = session.WithOrigin(ctx, group.SessionID, msg.Channel)
	ctx = memory.WithNotify(ctx, func(candidate memory.Candidate) error {
		adapter := g.adapter(msg.Channel)
		if adapter == nil {
			return fmt.Errorf("no owner adapter %q", msg.Channel)
		}
		return adapter.Send(ctx, msg.ChatID, fmt.Sprintf("%s change:\n%s", candidate.Kind, candidate.Diff))
	})
	var alertErr error
	newHistory, runErr := selected.Run(ctx, history, agent.Hooks{
		OnToolStart: func(use llm.ToolUse) {
			log.Info("tool", "channel", msg.Channel, "chat", msg.ChatID, "name", use.Name)
		},
		OnUsage: func(usage llm.Usage) {
			if runID != "" {
				if err := g.Observability.RecordUsage(ctx, runID, usage); err != nil {
					log.Error("record observability usage", "err", err)
				}
			}
			if g.Usage != nil && alertErr == nil {
				alertErr = g.Usage.Alert(ctx, group.SessionID, selected.Limits, time.Now(), func(deliverCtx context.Context, notice string) error {
					adapter := g.adapter(msg.Channel)
					if adapter == nil {
						return fmt.Errorf("no owner adapter %q", msg.Channel)
					}
					return adapter.Send(deliverCtx, msg.ChatID, notice)
				})
			}
		},
	})
	if runErr == nil && alertErr != nil {
		runErr = alertErr
	}
	if runID != "" {
		outcome := "ok"
		if runErr != nil {
			outcome = "error"
		}
		if err := g.Observability.Finish(context.WithoutCancel(ctx), runID, outcome); err != nil {
			log.Error("finish observability run", "err", err)
		}
	}

	for ; persisted < len(newHistory); persisted++ {
		if err := g.Sessions.AppendTurn(ctx, group.SessionID, newHistory[persisted]); err != nil {
			g.Log.Error("persist turn", "err", err)
			break
		}
	}
	// Turn-count reflection for conversations that never go idle (#59).
	// Already holds the conversation group lock via handle → converse.
	if g.ReflectEveryTurns > 0 && runErr == nil {
		if n, err := g.Sessions.TurnCount(ctx, group.SessionID); err == nil && n > 0 && n%g.ReflectEveryTurns == 0 {
			model := selected.Model
			if selected.UtilityModel != "" {
				model = selected.UtilityModel
			}
			if _, err := g.reflectSessionLocked(ctx, group.SessionID, newHistory, model, false); err != nil {
				log.Warn("reflect every turns", "err", err)
			}
		}
	}
	if runErr != nil {
		return "", runErr
	}

	// The reply is the final assistant message's text.
	for i := len(newHistory) - 1; i >= 0; i-- {
		if newHistory[i].Role == llm.RoleAssistant {
			return newHistory[i].Text(), nil
		}
	}
	return "", nil
}
