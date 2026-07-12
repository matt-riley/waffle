package spill

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/matt-riley/waffle/internal/llm"
)

// ExpandTool recovers truncated tool output from a spill id (#69).
type ExpandTool struct {
	Store *Store
}

func (t ExpandTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "expand_output",
		Description: "Recover bytes from a truncated tool result using the spill id shown in the truncation marker. Optional pattern greps; offset/limit page the content.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "spill id from the truncation marker"},
				"offset": {"type": "integer", "description": "byte offset (default 0)"},
				"limit": {"type": "integer", "description": "max bytes to return (default OutputLimit)"},
				"pattern": {"type": "string", "description": "if set, return matching lines/windows instead of a range"}
			},
			"required": ["id"]
		}`),
	}
}

func (t ExpandTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if t.Store == nil {
		return "", fmt.Errorf("spill store not configured")
	}
	var in struct {
		ID      string `json:"id"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	if in.ID == "" {
		return "", fmt.Errorf("id is required")
	}
	return t.Store.Expand(ctx, in.ID, in.Offset, in.Limit, in.Pattern)
}
