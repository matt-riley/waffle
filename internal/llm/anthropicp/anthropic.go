// Package anthropicp translates waffle's canonical LLM types to the
// Anthropic Messages API using the official Go SDK. ("p" for provider — the
// package name "anthropic" belongs to the SDK.)
package anthropicp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"

	"github.com/matt-riley/waffle/internal/llm"
)

const (
	// DefaultBaseURL is the Anthropic API's default endpoint.
	DefaultBaseURL = "https://api.anthropic.com"
	// DefaultModel is used when the request doesn't name one. The Opus 4.x
	// line is end-of-life; the current flagship is Opus 5.
	DefaultModel = "claude-opus-5"

	// minCacheableTokens is Anthropic's minimum cacheable prompt length. A
	// cache_control breakpoint whose prefix is shorter than this is refused
	// by the API, so breakpoints are only emitted when the estimated prefix
	// clears it.
	minCacheableTokens = 1024

	// charsPerToken is the character-per-token heuristic used to estimate
	// whether a breakpoint's prefix reaches minCacheableTokens. ~4
	// chars/token under-estimates code and JSON (which tokenize closer to
	// 6-7 chars/token), so the guard errs toward *skipping* a breakpoint
	// rather than requesting a cache the provider would refuse.
	charsPerToken = 4
)

// estimatedTokens approximates a text span's token count from its character
// length. Exact tokenization is unavailable client-side; the heuristic only
// gates cache-breakpoint placement (see minCacheableTokens).
func estimatedTokens(chars int) int {
	return chars / charsPerToken
}

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
	// Prompt caching: the stable prefix (system text plus the tool set) is
	// byte-identical across calls in a session, so the two breakpoints below
	// let Anthropic serve it from cache on every call after the first. The
	// guard estimates the prefix's token length because a breakpoint on a
	// prompt below the provider's minimum cacheable length is refused.
	// Messages are deliberately excluded: prepareContext summarises and
	// rewrites the history prefix, so a transcript breakpoint would sit
	// after content that varies between calls and cache nothing while still
	// paying the cache-write surcharge.
	if req.System != "" {
		block := anthropic.TextBlockParam{Text: req.System}
		if estimatedTokens(len(req.System)) >= minCacheableTokens {
			block.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		params.System = []anthropic.TextBlockParam{block}
	}
	// SystemExtra (the agent's per-run context summary) is a SECOND system
	// block without a breakpoint: it changes between calls, so a breakpoint
	// on it would cache nothing and only pay the cache-write surcharge.
	// Keeping it out of the System block also keeps that block's bytes
	// stable, which is what makes its breakpoint reusable (#247).
	if req.SystemExtra != "" {
		params.System = append(params.System, anthropic.TextBlockParam{Text: req.SystemExtra})
	}

	// estimatedChars is the character span of the stable prefix as it will
	// appear on the wire, up to and including the final tool definition. The
	// tools breakpoint only lands on the *last* tool — the one whose prefix
	// is the whole stable span — and only when that prefix clears the
	// minimum cacheable length. A breakpoint on an earlier tool would cache
	// a shorter prefix at the same surcharge for no benefit.
	estimatedChars := len(req.System)
	for _, t := range req.Tools {
		schema, err := toInputSchema(t.InputSchema)
		if err != nil {
			return params, fmt.Errorf("anthropic: tool %q schema: %w", t.Name, err)
		}
		estimatedChars += len(t.Name) + len(t.Description) + len(t.InputSchema)
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: schema,
		}})
	}
	// The tools breakpoint caches everything before it, including every
	// system block. A changing SystemExtra block between System and the
	// tools would therefore invalidate the prefix on every call, so the
	// breakpoint is suppressed whenever extra system text is present —
	// emitting it would only pay the write surcharge, never reuse a cache
	// entry (#247).
	if req.SystemExtra == "" && len(params.Tools) > 0 && estimatedTokens(estimatedChars) >= minCacheableTokens {
		params.Tools[len(params.Tools)-1].OfTool.CacheControl = anthropic.NewCacheControlEphemeralParam()
	}

	// seenToolUseIDs tracks tool_use ids in this request so tool_result blocks
	// can be validated against them. Interrupted turns and session resume can
	// persist a tool result without its preceding assistant tool_use; Anthropic
	// (and strict providers like Kimi K3) reject those orphaned tool_results
	// with 400, so they are dropped here instead of failing the turn.
	seenToolUseIDs := make(map[string]struct{})
	for _, m := range req.Messages {
		// Canonical layer owns size/type limits; the translator only
		// propagates them (never silently drops or truncates media).
		if err := llm.ValidateBlocks(m.Blocks); err != nil {
			return params, fmt.Errorf("anthropic: %w", err)
		}
		blocks, err := toBlocks(m.Blocks)
		if err != nil {
			return params, err
		}
		for _, b := range blocks {
			if b.OfToolUse != nil && b.OfToolUse.ID != "" {
				seenToolUseIDs[b.OfToolUse.ID] = struct{}{}
			}
		}
		filtered := blocks[:0]
		for _, b := range blocks {
			if b.OfToolResult != nil {
				id := b.OfToolResult.ToolUseID
				if id == "" {
					continue // orphaned tool_result with no tool_use id
				}
				if _, ok := seenToolUseIDs[id]; !ok {
					continue // tool_result whose tool_use is not in this request
				}
			}
			filtered = append(filtered, b)
		}
		blocks = filtered
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
			if len(b.ToolResult.Blocks) == 0 {
				out = append(out, anthropic.NewToolResultBlock(b.ToolResult.ToolUseID, b.ToolResult.Content, b.ToolResult.IsError))
				continue
			}
			content, err := toToolResultContent(b.ToolResult.Blocks)
			if err != nil {
				return nil, err
			}
			out = append(out, anthropic.ContentBlockParamUnion{OfToolResult: &anthropic.ToolResultBlockParam{
				ToolUseID: b.ToolResult.ToolUseID,
				Content:   content,
				IsError:   anthropic.Bool(b.ToolResult.IsError),
			}})
		case llm.BlockImage:
			block, err := toImageBlock(b)
			if err != nil {
				return nil, err
			}
			out = append(out, block)
		case llm.BlockDocument:
			block, err := toDocumentBlock(b)
			if err != nil {
				return nil, err
			}
			out = append(out, block)
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

// toImageBlock maps a canonical image block to the SDK's image param. The
// canonical layer has already validated the source, so every branch here is
// a straight wire mapping.
func toImageBlock(b llm.Block) (anthropic.ContentBlockParamUnion, error) {
	switch b.Source.Type {
	case llm.SourceBase64:
		return anthropic.NewImageBlockBase64(b.Source.MediaType, b.Source.Data), nil
	case llm.SourceURL:
		return anthropic.NewImageBlock(anthropic.URLImageSourceParam{URL: b.Source.URL}), nil
	default:
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("anthropic: image block with unknown source type %q", b.Source.Type)
	}
}

// toDocumentBlock maps a canonical document block to the SDK's document
// param. PDFs and text types map to the SDK's native sources; other
// documented office formats ride the base64 source (the API accepts them
// with their media type).
func toDocumentBlock(b llm.Block) (anthropic.ContentBlockParamUnion, error) {
	switch b.Source.Type {
	case llm.SourceBase64:
		if strings.HasPrefix(b.Source.MediaType, "text/") {
			decoded, err := base64.StdEncoding.DecodeString(b.Source.Data)
			if err != nil {
				return anthropic.ContentBlockParamUnion{}, fmt.Errorf("anthropic: document block has invalid base64 data: %w", err)
			}
			return anthropic.NewDocumentBlock(anthropic.PlainTextSourceParam{
				Data:      string(decoded),
				MediaType: constant.TextPlain(b.Source.MediaType),
			}), nil
		}
		// application/pdf and office formats: base64 source with the media
		// type carried through. The SDK's param is named for PDFs but its
		// media_type marshals whatever constant value it holds.
		return anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{
			Data:      b.Source.Data,
			MediaType: constant.ApplicationPDF(b.Source.MediaType),
		}), nil
	case llm.SourceURL:
		return anthropic.NewDocumentBlock(anthropic.URLPDFSourceParam{URL: b.Source.URL}), nil
	default:
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("anthropic: document block with unknown source type %q", b.Source.Type)
	}
}

// toToolResultContent maps a tool result's structured blocks to the SDK's
// tool_result content union. Text, image, and document blocks are all
// representable inside an Anthropic tool result.
func toToolResultContent(blocks []llm.Block) ([]anthropic.ToolResultBlockParamContentUnion, error) {
	var out []anthropic.ToolResultBlockParamContentUnion
	for _, b := range blocks {
		switch b.Type {
		case llm.BlockText:
			out = append(out, anthropic.ToolResultBlockParamContentUnion{OfText: &anthropic.TextBlockParam{Text: b.Text}})
		case llm.BlockImage:
			block, err := toImageBlock(b)
			if err != nil {
				return nil, err
			}
			out = append(out, anthropic.ToolResultBlockParamContentUnion{OfImage: block.OfImage})
		case llm.BlockDocument:
			block, err := toDocumentBlock(b)
			if err != nil {
				return nil, err
			}
			out = append(out, anthropic.ToolResultBlockParamContentUnion{OfDocument: block.OfDocument})
		default:
			return nil, fmt.Errorf("anthropic: unsupported block type %q in tool result", b.Type)
		}
	}
	return out, nil
}

func fromMessage(msg anthropic.Message) (*llm.Response, error) {
	resp := &llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant},
		Usage: llm.Usage{
			InputTokens:              int(msg.Usage.InputTokens),
			OutputTokens:             int(msg.Usage.OutputTokens),
			CacheCreationInputTokens: int(msg.Usage.CacheCreationInputTokens),
			CacheReadInputTokens:     int(msg.Usage.CacheReadInputTokens),
			// The translator knows the provider, so the usage it reports is
			// attributed to it for per-provider budget pricing (#247).
			Provider: "anthropic",
		},
	}
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			out := llm.Block{Type: llm.BlockText, Text: b.Text}
			for i, citation := range b.Citations {
				if translated := translateCitation(citation, i+1); translated != nil {
					out.Citations = append(out.Citations, *translated)
				}
			}
			resp.Message.Blocks = append(resp.Message.Blocks, out)
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

// translateCitation maps one provider citation to waffle's provider-neutral
// form (#479). Web-search citations become safe web sources (their URL is
// restricted to http/https by the projection layer); document citations
// (char/page/content-block locations) become opaque workspace-local
// resources using the provider's file id — never an absolute path. Every
// translated citation gets a deterministic, provider-neutral ID so persisted
// turns and streamed source events share stable source identifiers. Unknown
// citation shapes are dropped so the contract stays additive.
func translateCitation(citation anthropic.TextCitationUnion, sequence int) *llm.Citation {
	switch v := citation.AsAny().(type) {
	case anthropic.CitationsWebSearchResultLocation:
		label := strings.TrimSpace(v.Title)
		if label == "" {
			label = strings.TrimSpace(v.URL)
		}
		if label == "" {
			label = "Web source"
		}
		return &llm.Citation{
			ID: citationID(sequence), Kind: llm.CitationWeb, Label: label, URL: strings.TrimSpace(v.URL),
			Snippet: boundedCitationSnippet(v.CitedText), Provenance: "provider citation",
		}
	case anthropic.CitationCharLocation:
		return documentCitation(sequence, v.DocumentTitle, v.FileID, v.CitedText)
	case anthropic.CitationPageLocation:
		return documentCitation(sequence, v.DocumentTitle, v.FileID, v.CitedText)
	case anthropic.CitationContentBlockLocation:
		return documentCitation(sequence, v.DocumentTitle, v.FileID, v.CitedText)
	default:
		return nil
	}
}

func documentCitation(sequence int, title, fileID, citedText string) *llm.Citation {
	label := strings.TrimSpace(title)
	if label == "" {
		label = "Workspace document"
	}
	return &llm.Citation{
		ID: citationID(sequence), Kind: llm.CitationWorkspace, Label: label, Resource: strings.TrimSpace(fileID),
		Snippet: boundedCitationSnippet(citedText), Provenance: "provider citation",
	}
}

// citationID builds a deterministic, provider-neutral source ID from the
// citation's position within the response (c1, c2, …). IDs are opaque
// display identifiers; provider file ids and URLs never leak into them.
func citationID(sequence int) string {
	return fmt.Sprintf("c%d", sequence)
}

// boundedCitationSnippet caps a cited-text excerpt so hostile or oversized
// provider snippets cannot bloat persisted turns or the Desk drawer.
func boundedCitationSnippet(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 280 {
		return text[:280] + "…"
	}
	return text
}
