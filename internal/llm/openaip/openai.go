// Package openaip translates waffle's canonical LLM types to the OpenAI
// Chat Completions dialect. This one provider covers OpenAI itself plus the
// long tail of compatible endpoints — OpenRouter, Ollama, Gemini, and vLLM —
// which is what makes waffle's no-lock-in principle real (docs/plan.md).
package openaip

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
)

// DefaultBaseURL is the OpenAI API's default versioned endpoint.
const DefaultBaseURL = "https://api.openai.com/v1"

// maxAccumulatedBytes caps text and tool call argument accumulation in
// readStream to prevent unbounded memory growth from verbose or malicious
// streams. Text and tool call arguments are budgeted separately (each up to
// this cap) so that verbose text cannot starve a legitimate tool call.
// It can be lowered in tests. The default is 2 MiB; Complete may tighten it
// based on req.MaxTokens.
var maxAccumulatedBytes = 2 * 1024 * 1024

// Provider calls an OpenAI-compatible chat completions endpoint.
type Provider struct {
	APIKey  string
	BaseURL string // e.g. https://api.openai.com/v1 or http://localhost:11434/v1
	Client  *http.Client
}

// New builds a Provider. baseURL must not be empty.
func New(apiKey, baseURL string) *Provider {
	return &Provider{
		APIKey:  apiKey,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{Timeout: 10 * time.Minute},
	}
}

// Wire types for the chat completions dialect.

type wireMessage struct {
	Role       string         `json:"role"`
	Content    wireContent    `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// wireContent is a message body that is either a plain string (the
// historical shape) or an array of typed content parts (needed once a
// message carries an image). The string form is preserved so text-only
// requests keep the exact wire shape.
type wireContent struct {
	Text  string
	Parts []wireContentPart
}

func (c wireContent) MarshalJSON() ([]byte, error) {
	if c.Parts != nil {
		return json.Marshal(c.Parts)
	}
	return json.Marshal(c.Text)
}

// wireContentPart is one item of a content-part array. Only text and
// image_url parts exist in this dialect; documents have no equivalent and
// are rejected up front (see translateMessage).
type wireContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

type wireImageURL struct {
	URL string `json:"url"`
}

type wireToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type wireRequest struct {
	Model         string             `json:"model"`
	Messages      []wireMessage      `json:"messages"`
	Tools         []wireTool         `json:"tools,omitempty"`
	Stream        bool               `json:"stream"`
	StreamOptions *wireStreamOptions `json:"stream_options,omitempty"`
	MaxTok        int                `json:"max_tokens,omitempty"`
}

// wireStreamOptions asks the backend to include a final usage-only chunk in
// the stream; spec-compliant OpenAI endpoints omit usage entirely otherwise.
type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type wireChunk struct {
	Choices []struct {
		// Delta carries streamed deltas, which are always plain strings on
		// the wire (requests use wireMessage, whose content may be parts).
		Delta        wireDelta `json:"delta"`
		FinishReason string    `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type wireDelta struct {
	Content   string         `json:"content"`
	ToolCalls []wireToolCall `json:"tool_calls"`
}

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.Request, onEvent llm.StreamFunc) (resp *llm.Response, err error) {
	wire, err := p.toWire(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	httpResp, err := p.client().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	defer func() {
		if cerr := httpResp.Body.Close(); err == nil {
			err = cerr
		}
	}()

	if httpResp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, fmt.Errorf("openai: %s: %s", httpResp.Status, strings.TrimSpace(string(msg)))
	}

	maxBytes := maxAccumulatedBytes
	if req.MaxTokens > 0 {
		// Conservative byte estimate per output token (UTF-8 can be higher
		// ratio in practice); tighten relative to MaxTokens but never exceed
		// the hard 2MiB memory safety limit. Using *8 to avoid truncating
		// valid output that respects the token budget.
		if est := req.MaxTokens * 8; est > 0 && est < maxBytes {
			maxBytes = est
		}
	}
	return p.readStream(httpResp.Body, onEvent, maxBytes)
}

func (p *Provider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

func (p *Provider) toWire(req llm.Request) (wireRequest, error) {
	w := wireRequest{Model: req.Model, Stream: true, MaxTok: req.MaxTokens}
	if w.Stream {
		w.StreamOptions = &wireStreamOptions{IncludeUsage: true}
	}
	if req.System != "" {
		w.Messages = append(w.Messages, wireMessage{Role: "system", Content: wireContent{Text: req.System}})
	}
	for _, m := range req.Messages {
		// Canonical layer owns size/type limits; the translator only
		// propagates them (never silently drops or truncates media).
		if err := llm.ValidateBlocks(m.Blocks); err != nil {
			return wireRequest{}, fmt.Errorf("openai: %w", err)
		}
		msgs, err := translateMessage(m)
		if err != nil {
			return wireRequest{}, err
		}
		w.Messages = append(w.Messages, msgs...)
	}
	for _, t := range req.Tools {
		wt := wireTool{Type: "function"}
		wt.Function.Name = t.Name
		wt.Function.Description = t.Description
		wt.Function.Parameters = t.InputSchema
		w.Tools = append(w.Tools, wt)
	}
	return w, nil
}

// translateMessage maps one canonical message to one or more wire messages:
// tool results become role:"tool" messages (which must directly follow the
// assistant turn that requested them), and thinking blocks are dropped —
// this dialect has no equivalent.
//
// Images are representable only as image_url content parts inside user
// messages. Documents have no universal equivalent in this dialect and
// images cannot ride tool messages (their content is a plain string), so
// those combinations fail with an explicit, actionable error instead of
// silently dropping content: a user who sends a photo must never get an
// answer computed without it.
func translateMessage(m llm.Message) ([]wireMessage, error) {
	if m.Role == llm.RoleAssistant {
		out := wireMessage{Role: "assistant"}
		for _, b := range m.Blocks {
			switch b.Type {
			case llm.BlockText:
				out.Content.Text += b.Text
			case llm.BlockToolUse:
				tc := wireToolCall{ID: b.ToolUse.ID, Type: "function"}
				tc.Function.Name = b.ToolUse.Name
				tc.Function.Arguments = string(b.ToolUse.Input)
				out.ToolCalls = append(out.ToolCalls, tc)
			}
		}
		return []wireMessage{out}, nil
	}

	var msgs []wireMessage
	// userContent accumulates one user message's blocks so text and image
	// parts keep their order inside a single content array. sawImage is set
	// the moment an image appears: from then on the accumulated text is
	// emitted as parts too, so a message that never had an image keeps the
	// historical plain-string content form.
	var userContent []wireContentPart
	var text strings.Builder
	sawImage := false
	flushUser := func() {
		if text.Len() > 0 {
			if sawImage {
				userContent = append(userContent, wireContentPart{Type: "text", Text: text.String()})
			} else {
				msgs = append(msgs, wireMessage{Role: "user", Content: wireContent{Text: text.String()}})
			}
			text.Reset()
		}
		if len(userContent) > 0 {
			msgs = append(msgs, wireMessage{Role: "user", Content: wireContent{Parts: userContent}})
			userContent = nil
			sawImage = false
		}
	}
	for _, b := range m.Blocks {
		switch b.Type {
		case llm.BlockToolResult:
			// Preserve the historical wire order: tool messages are emitted
			// in place and pending user text after them, unless an image
			// forces parts mode (then the text must flush first so the
			// content-part array keeps its order).
			if sawImage {
				flushUser()
			}
			content, err := toolResultContent(b.ToolResult)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, wireMessage{Role: "tool", ToolCallID: b.ToolResult.ToolUseID, Content: wireContent{Text: content}})
		case llm.BlockImage:
			if text.Len() > 0 {
				userContent = append(userContent, wireContentPart{Type: "text", Text: text.String()})
				text.Reset()
			}
			sawImage = true
			userContent = append(userContent, wireContentPart{Type: "image_url", ImageURL: &wireImageURL{URL: imageURL(b)}})
		case llm.BlockDocument:
			// No universal document part exists in the chat-completions
			// dialect. Fail loudly rather than let the user's PDF silently
			// vanish from the prompt (see package doc decision note).
			return nil, fmt.Errorf("openai: cannot represent block type %q in this dialect (chat completions has no document content part); convert the document to text first, or use a provider that supports documents", llm.BlockDocument)
		case llm.BlockText:
			text.WriteString(b.Text)
		}
	}
	flushUser()
	return msgs, nil
}

// toolResultContent flattens a tool result to the plain string this dialect
// requires for role:"tool" messages. Text blocks are concatenated; media
// blocks have no string form and fail explicitly.
func toolResultContent(tr *llm.ToolResult) (string, error) {
	if len(tr.Blocks) == 0 {
		content := tr.Content
		if tr.IsError {
			content = "ERROR: " + content
		}
		return content, nil
	}
	var b strings.Builder
	for _, bl := range tr.Blocks {
		switch bl.Type {
		case llm.BlockText:
			b.WriteString(bl.Text)
		case llm.BlockImage:
			return "", fmt.Errorf("openai: cannot represent block type %q inside a tool result (role tool content is a plain string); include the image in a user message instead, or use a provider that supports image tool results", llm.BlockImage)
		case llm.BlockDocument:
			return "", fmt.Errorf("openai: cannot represent block type %q inside a tool result (role tool content is a plain string); convert the document to text first, or use a provider that supports documents", llm.BlockDocument)
		}
	}
	if tr.IsError {
		return "ERROR: " + b.String(), nil
	}
	return b.String(), nil
}

// imageURL renders a canonical image source in the image_url form this
// dialect requires: a data URI for inline base64, the URL itself for URL
// sources.
func imageURL(b llm.Block) string {
	if b.Source.Type == llm.SourceURL {
		return b.Source.URL
	}
	return "data:" + b.Source.MediaType + ";base64," + b.Source.Data
}

func (p *Provider) readStream(body io.Reader, onEvent llm.StreamFunc, maxBytes int) (*llm.Response, error) {
	if maxBytes <= 0 {
		maxBytes = maxAccumulatedBytes
	}
	resp := &llm.Response{Message: llm.Message{Role: llm.RoleAssistant}, StopReason: llm.StopOther}
	var text strings.Builder
	toolCalls := map[int]*wireToolCall{}
	// truncatedCalls tracks tool calls whose arguments were cut off by the
	// byte cap; they must not be emitted (their args would be corrupt JSON).
	truncatedCalls := map[int]bool{}
	// Text and tool call arguments spend separate budgets so verbose text
	// cannot starve a legitimate tool call of room for its arguments.
	hitTextCap := false
	hitToolCap := false
	textSize := 0
	toolSize := 0

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var chunk wireChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("openai: bad stream chunk: %w", err)
		}
		if chunk.Usage != nil {
			resp.Usage.InputTokens = chunk.Usage.PromptTokens
			resp.Usage.OutputTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]

		if choice.Delta.Content != "" {
			if !hitTextCap {
				d := choice.Delta.Content
				room := maxBytes - textSize
				if len(d) > room {
					if room > 0 {
						d = d[:room]
						text.WriteString(d)
						textSize += len(d)
						if onEvent != nil {
							onEvent(llm.Event{Type: llm.EventTextDelta, Text: d})
						}
					}
					hitTextCap = true
				} else if room > 0 {
					text.WriteString(d)
					textSize += len(d)
					if onEvent != nil {
						onEvent(llm.Event{Type: llm.EventTextDelta, Text: d})
					}
				}
			}
		}
		for _, tc := range choice.Delta.ToolCalls {
			acc, ok := toolCalls[tc.Index]
			if !ok {
				acc = &wireToolCall{Index: tc.Index}
				toolCalls[tc.Index] = acc
			}
			if tc.ID != "" {
				acc.ID = tc.ID
			}
			if tc.Function.Name != "" {
				acc.Function.Name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				if hitToolCap {
					// Args arrived after the cap and were skipped: this
					// call's arguments are incomplete.
					truncatedCalls[tc.Index] = true
					continue
				}
				arg := tc.Function.Arguments
				room := maxBytes - toolSize
				if len(arg) > room {
					if room > 0 {
						arg = arg[:room]
						acc.Function.Arguments += arg
						toolSize += len(arg)
					}
					hitToolCap = true
					truncatedCalls[tc.Index] = true
				} else {
					acc.Function.Arguments += arg
					toolSize += len(arg)
				}
			}
		}
		switch choice.FinishReason {
		case "stop":
			resp.StopReason = llm.StopEndTurn
		case "tool_calls":
			resp.StopReason = llm.StopToolUse
		case "length":
			resp.StopReason = llm.StopMaxTokens
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("openai: read stream: %w", err)
	}

	hitCap := hitTextCap || hitToolCap
	if hitToolCap {
		// Drop only the tool calls whose arguments were truncated (their args
		// may be invalid JSON, causing downstream unmarshal/exec errors in
		// agent). Complete tool calls are preserved so a legitimate call is
		// not silently discarded. Only when no complete call remains do we
		// override the stop reason so the caller does not treat this as a
		// tool request.
		for i := range truncatedCalls {
			delete(toolCalls, i)
		}
		if len(toolCalls) == 0 && resp.StopReason == llm.StopToolUse {
			resp.StopReason = llm.StopEndTurn
		}
	}

	if text.Len() > 0 {
		resp.Message.Blocks = append(resp.Message.Blocks, llm.Block{Type: llm.BlockText, Text: text.String()})
	}
	indexes := make([]int, 0, len(toolCalls))
	for i := range toolCalls {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		tc := toolCalls[i]
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		resp.Message.Blocks = append(resp.Message.Blocks, llm.Block{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(args),
		}})
	}
	if hitCap {
		// Append a warning block (as text) so callers see a bounded partial
		// response plus an explicit indicator, matching the review guidance.
		resp.Message.Blocks = append(resp.Message.Blocks, llm.Block{
			Type: llm.BlockText,
			Text: "\n[WARNING: output truncated due to size limit]",
		})
	}
	return resp, nil
}
