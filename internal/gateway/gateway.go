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
	"sync"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/channel"
	"github.com/matt-riley/waffle/internal/entity"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/session"
)

// Gateway wires adapters to the agent runtime.
type Gateway struct {
	Agent    *agent.Agent
	Entities *entity.Store
	Sessions *session.Store
	Adapters []channel.Adapter
	Log      *slog.Logger

	mu     sync.Mutex
	groups map[string]*sync.Mutex // per-conversation serialization
}

// Run starts every adapter and processes inbound messages until ctx ends.
func (g *Gateway) Run(ctx context.Context) error {
	if g.Log == nil {
		g.Log = slog.Default()
	}
	g.groups = make(map[string]*sync.Mutex)
	if len(g.Adapters) == 0 {
		return errors.New("gateway: no channels configured (enable one in config.toml)")
	}

	inbound := make(chan channel.Message, 64)
	var wg sync.WaitGroup
	for _, a := range g.Adapters {
		wg.Add(1)
		go func(a channel.Adapter) {
			defer wg.Done()
			if err := a.Run(ctx, inbound); err != nil {
				g.Log.Error("adapter stopped", "channel", a.Name(), "err", err)
			}
		}(a)
		g.Log.Info("channel up", "channel", a.Name())
	}

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case msg := <-inbound:
			// Handle concurrently across conversations, serially within
			// one (the per-group lock).
			go g.handle(ctx, msg)
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

func (g *Gateway) groupLock(key string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	l, ok := g.groups[key]
	if !ok {
		l = &sync.Mutex{}
		g.groups[key] = l
	}
	return l
}

func (g *Gateway) handle(ctx context.Context, msg channel.Message) {
	log := g.Log.With("channel", msg.Channel, "chat", msg.ChatID)
	adapter := g.adapter(msg.Channel)
	if adapter == nil {
		log.Error("message from unknown adapter")
		return
	}

	// Who is this? Not the owner → pairing code, nothing more. The code is
	// only redeemable via the host CLI, so strangers can't self-approve.
	if _, err := g.Entities.Identify(ctx, msg.Channel, msg.SenderID); err != nil {
		if !errors.Is(err, entity.ErrUnknownSender) {
			log.Error("identify", "err", err)
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

	lock := g.groupLock(msg.Channel + "\x00" + msg.ChatID)
	lock.Lock()
	defer lock.Unlock()

	reply, err := g.converse(ctx, msg)
	if err != nil {
		log.Error("agent run", "err", err)
		reply = fmt.Sprintf("something went wrong: %v", err)
	}
	if reply == "" {
		return
	}
	if err := adapter.Send(ctx, msg.ChatID, reply); err != nil {
		log.Error("send reply", "err", err)
	}
}

// converse routes one owner message through the conversation's session.
func (g *Gateway) converse(ctx context.Context, msg channel.Message) (string, error) {
	group, err := g.Entities.GroupFor(ctx, msg.Channel, msg.ChatID)
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
	newHistory, runErr := g.Agent.Run(ctx, history, agent.Hooks{
		OnToolStart: func(use llm.ToolUse) {
			g.Log.Info("tool", "channel", msg.Channel, "chat", msg.ChatID, "name", use.Name)
		},
	})

	for ; persisted < len(newHistory); persisted++ {
		if err := g.Sessions.AppendTurn(ctx, group.SessionID, newHistory[persisted]); err != nil {
			g.Log.Error("persist turn", "err", err)
			break
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
