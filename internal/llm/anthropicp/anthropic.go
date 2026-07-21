// Package anthropicp translates waffle's canonical LLM types to the
// Anthropic Messages API using the official Go SDK. ("p" for provider — the
// package name "anthropic" belongs to the SDK.)
package anthropicp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/matt-riley/waffle/internal/llm"
)

const (
	// DefaultBaseURL is the Anthropic API's default endpoint.
	DefaultBaseURL = "https://api.anthropic.com"
	// DefaultModel is used when the request doesn't name one.
	DefaultModel = "claude-opus-4-8"
)

// Provider calls the Anthropic Messages API.
type Provider struct {
	client anthropic.Client
}

// New builds a Provider. apiKey and baseURL may be empty, in which case the
// SDK falls back to ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL.
func New(apiKey, baseURL string) *Provider {
	var opts []option.RequestOption
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &Provider{client: anthropic.NewClient(opts...)}
}

// Complete implements llm.Provider with a streaming request.
func (p *Provider) Complete(ctx context.Context, req llm.Request, onEvent llm.StreamFunc) (*llm.Response, error) {
	params, err := toParams(req)
	if err != nil {
		return nil, err
	}

	stream := p.client.Messages.NewStreaming(ctx, params)
	// Release the HTTP response body (and pooled connection) on every path,
	// including early accumulate errors and stream.Err() failures.
	defer func() { _ = stream.Close() }()
	msg := anthropic.Message{}
	for stream.Next() {
		event := stream.Current()
		if err := msg.Accumulate(event); err != nil {
			return nil, fmt.Errorf("anthropic: accumulate stream: %w", err)
		}
		if onEvent != nil {
			if delta, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
				if text, ok := delta.Delta.AsAny().(anthropic.TextDelta); ok {
					onEvent(llm.Event{Type: llm.EventTextDelta, Text: text.Text})
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	return fromMessage(msg)
}

func toParams(req llm.Request) (anthropic.MessageNewParams, error) {
	model := req.Model
	if model == "" {
		model = DefaultModel
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 64000
	}

	params := anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: int64(maxTokens),
		// Adaptive thinking: the model decides when and how deeply to
		// reason. The recommended mode on Claude 4.6+ models.
		Thinking: anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
		},
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}

	for _, t := range req.Tools {
		schema, err := toInputSchema(t.InputSchema)
		if err != nil {
			return params, fmt.Errorf("anthropic: tool %q schema: %w", t.Name, err)
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: schema,
		}})
	}

	for _, m := range req.Messages {
		blocks, err := toBlocks(m.Blocks)
		if err != nil {
			return params, err
		}
		if len(blocks) == 0 {
			continue
		}
		role := anthropic.MessageParamRoleUser
		if m.Role == llm.RoleAssistant {
			role = anthropic.MessageParamRoleAssistant
		}
		params.Messages = append(params.Messages, anthropic.MessageParam{Role: role, Content: blocks})
	}
	return params, nil
}

func toInputSchema(raw json.RawMessage) (anthropic.ToolInputSchemaParam, error) {
	var schema struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return anthropic.ToolInputSchemaParam{}, err
	}
	return anthropic.ToolInputSchemaParam{
		Properties: schema.Properties,
		Required:   schema.Required,
	}, nil
}

func toBlocks(blocks []llm.Block) ([]anthropic.ContentBlockParamUnion, error) {
	var out []anthropic.ContentBlockParamUnion
	for _, b := range blocks {
		switch b.Type {
		case llm.BlockText:
			if b.Text == "" {
				continue
			}
			out = append(out, anthropic.NewTextBlock(b.Text))
		case llm.BlockToolUse:
			out = append(out, anthropic.ContentBlockParamUnion{OfToolUse: &anthropic.ToolUseBlockParam{
				ID:    b.ToolUse.ID,
				Name:  b.ToolUse.Name,
				Input: b.ToolUse.Input,
			}})
		case llm.BlockToolResult:
			out = append(out, anthropic.NewToolResultBlock(b.ToolResult.ToolUseID, b.ToolResult.Content, b.ToolResult.IsError))
		case llm.BlockThinking:
			// Replay exactly as received — the API rejects modified blocks.
			out = append(out, anthropic.ContentBlockParamUnion{OfThinking: &anthropic.ThinkingBlockParam{
				Thinking:  b.Text,
				Signature: b.Signature,
			}})
		case llm.BlockRedactedThinking:
			out = append(out, anthropic.ContentBlockParamUnion{OfRedactedThinking: &anthropic.RedactedThinkingBlockParam{
				Data: b.Data,
			}})
		default:
			return nil, fmt.Errorf("anthropic: unsupported block type %q", b.Type)
		}
	}
	return out, nil
}

func fromMessage(msg anthropic.Message) (*llm.Response, error) {
	resp := &llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant},
		Usage: llm.Usage{
			InputTokens:  int(msg.Usage.InputTokens),
			OutputTokens: int(msg.Usage.OutputTokens),
		},
	}
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			resp.Message.Blocks = append(resp.Message.Blocks, llm.Block{Type: llm.BlockText, Text: b.Text})
		case anthropic.ToolUseBlock:
			resp.Message.Blocks = append(resp.Message.Blocks, llm.Block{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{
				ID:    b.ID,
				Name:  b.Name,
				Input: json.RawMessage(b.JSON.Input.Raw()),
			}})
		case anthropic.ThinkingBlock:
			resp.Message.Blocks = append(resp.Message.Blocks, llm.Block{Type: llm.BlockThinking, Text: b.Thinking, Signature: b.Signature})
		case anthropic.RedactedThinkingBlock:
			resp.Message.Blocks = append(resp.Message.Blocks, llm.Block{Type: llm.BlockRedactedThinking, Data: b.Data})
		}
	}

	switch msg.StopReason {
	case anthropic.StopReasonEndTurn:
		resp.StopReason = llm.StopEndTurn
	case anthropic.StopReasonToolUse:
		resp.StopReason = llm.StopToolUse
	case anthropic.StopReasonMaxTokens:
		resp.StopReason = llm.StopMaxTokens
	case anthropic.StopReasonRefusal:
		resp.StopReason = llm.StopRefusal
	default:
		resp.StopReason = llm.StopOther
	}
	return resp, nil
}
