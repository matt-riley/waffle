// Package llmtest provides reusable fake LLM providers for offline tests
// and the eval harness (#63).
package llmtest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/matt-riley/waffle/internal/llm"
)

// Script returns canned Complete responses in order and records requests.
// Safe for sequential agent Runs; not intended for parallel Complete calls
// without external synchronization.
type Script struct {
	mu sync.Mutex

	// Responses are consumed in order. When exhausted, Complete returns
	// ErrExhausted unless Default is set.
	Responses []llm.Response
	// Default, when non-nil, is returned after Responses are exhausted.
	Default *llm.Response

	// Requests is every Complete request observed.
	Requests []llm.Request
	// Models is req.Model for each call (convenience for utility_model tests).
	Models []string
	// Calls is the number of Complete invocations.
	Calls int
}

// ErrExhausted is returned when Responses are consumed and Default is nil.
var ErrExhausted = errors.New("llmtest: scripted responses exhausted")

// Complete implements llm.Provider.
func (s *Script) Complete(ctx context.Context, req llm.Request, onEvent llm.StreamFunc) (*llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls++
	s.Requests = append(s.Requests, req)
	s.Models = append(s.Models, req.Model)

	var resp llm.Response
	if len(s.Responses) > 0 {
		resp = s.Responses[0]
		s.Responses = s.Responses[1:]
	} else if s.Default != nil {
		resp = *s.Default
	} else {
		return nil, ErrExhausted
	}
	if onEvent != nil {
		for _, b := range resp.Message.Blocks {
			if b.Type == llm.BlockText {
				onEvent(llm.Event{Type: llm.EventTextDelta, Text: b.Text})
			}
		}
	}
	return &resp, nil
}

// Text returns an end-turn assistant text response.
func Text(s string) llm.Response {
	return llm.Response{
		StopReason: llm.StopEndTurn,
		Message: llm.Message{
			Role:   llm.RoleAssistant,
			Blocks: []llm.Block{{Type: llm.BlockText, Text: s}},
		},
	}
}

// ToolCall returns a tool-use stop response.
func ToolCall(name, id, input string) llm.Response {
	return llm.Response{
		StopReason: llm.StopToolUse,
		Message: llm.Message{
			Role: llm.RoleAssistant,
			Blocks: []llm.Block{{
				Type:    llm.BlockToolUse,
				ToolUse: &llm.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)},
			}},
		},
	}
}
