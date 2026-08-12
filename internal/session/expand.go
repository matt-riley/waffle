package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

// ExpandContextTool fetches verbatim session turns by 1-based inclusive range
// so the model can recover content that was summarized away (#61).
type ExpandContextTool struct {
	Sessions *Store
}

func (t ExpandContextTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "expand_context",
		Description: "Fetch verbatim session turns by 1-based inclusive range (from context summary handles like turns=1-12). Defaults to the current session.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"from": {"type": "integer", "description": "first turn (1-based, inclusive)"},
				"to": {"type": "integer", "description": "last turn (1-based, inclusive)"},
				"session_id": {"type": "string", "description": "optional session id; default is the active session"}
			},
			"required": ["from", "to"]
		}`),
	}
}

func (t ExpandContextTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if t.Sessions == nil {
		return "", fmt.Errorf("session store not configured")
	}
	var in struct {
		From      int    `json:"from"`
		To        int    `json:"to"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	if in.From < 1 || in.To < in.From {
		return "", fmt.Errorf("invalid range from=%d to=%d", in.From, in.To)
	}
	sid := strings.TrimSpace(in.SessionID)
	if sid == "" {
		sid = IDFromContext(ctx)
	}
	if sid == "" {
		return "", fmt.Errorf("no session id")
	}
	// Cap expansion so a single tool call cannot dump the entire history.
	if in.To-in.From+1 > 40 {
		return "", fmt.Errorf("range too large (max 40 turns per call)")
	}
	turns, err := t.Sessions.TurnsRange(ctx, sid, in.From, in.To)
	if err != nil {
		return "", err
	}
	if len(turns) == 0 {
		return "no turns in that range", nil
	}
	var b strings.Builder
	for i, m := range turns {
		seq := in.From + i
		fmt.Fprintf(&b, "--- turn %d (%s) ---\n", seq, m.Role)
		text := m.Text()
		for _, bl := range m.Blocks {
			if bl.Type == llm.BlockToolResult && bl.ToolResult != nil {
				text += "\n[tool_result] " + bl.ToolResult.Content
				for _, inner := range bl.ToolResult.Blocks {
					if inner.Type == llm.BlockText {
						text += " " + inner.Text
					}
				}
			}
			if bl.Type == llm.BlockToolUse && bl.ToolUse != nil {
				text += fmt.Sprintf("\n[tool_use %s] %s", bl.ToolUse.Name, string(bl.ToolUse.Input))
			}
		}
		b.WriteString(tool.Truncate(text, 8*1024))
		b.WriteByte('\n')
	}
	return tool.Truncate(b.String(), tool.OutputLimit), nil
}

// TurnsRange returns turns with 1-based inclusive sequence numbers.
func (s *Store) TurnsRange(ctx context.Context, sessionID string, from, to int) ([]llm.Message, error) {
	if from < 1 || to < from {
		return nil, fmt.Errorf("invalid range")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, blocks FROM turns
		WHERE session_id = ? AND seq >= ? AND seq <= ?
		ORDER BY seq`, sessionID, from, to)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var msgs []llm.Message
	for rows.Next() {
		var role, blocks string
		if err := rows.Scan(&role, &blocks); err != nil {
			return nil, err
		}
		msg := llm.Message{Role: llm.Role(role)}
		if err := json.Unmarshal([]byte(blocks), &msg.Blocks); err != nil {
			return nil, fmt.Errorf("corrupt turn: %w", err)
		}
		msgs = append(msgs, msg)
	}
	return msgs, rows.Err()
}
