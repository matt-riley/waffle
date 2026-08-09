package chat

import (
	"encoding/json"

	"github.com/matt-riley/waffle/internal/llm"
)

// RedactFunc scrubs one text value. Exact-value redactors (agent.Redact, the
// dashboard's ChatClients redactor) and residual format scrubbers both fit.
type RedactFunc func(string) string

// RedactEvent returns a copy of event with every projected text field passed
// through redact. Shared by chat runtime and any surface projecting events
// (#289).
func RedactEvent(event Event, redact RedactFunc) Event {
	event.Text = redact(event.Text)
	event.ToolName = redact(event.ToolName)
	if event.State != nil {
		state := RedactState(*event.State, redact)
		event.State = &state
	}
	return event
}

// RedactResult returns a copy of result with every projected text field
// passed through redact.
func RedactResult(result Result, redact RedactFunc) Result {
	result.Title = redact(result.Title)
	result.Text = redact(result.Text)
	for i := range result.Models {
		result.Models[i].Alias = redact(result.Models[i].Alias)
		result.Models[i].Provider = redact(result.Models[i].Provider)
		result.Models[i].Upstream = redact(result.Models[i].Upstream)
	}
	for i := range result.Sessions {
		result.Sessions[i].Title = redact(result.Sessions[i].Title)
		result.Sessions[i].Summary = redact(result.Sessions[i].Summary)
		result.Sessions[i].ModelAlias = redact(result.Sessions[i].ModelAlias)
	}
	for i := range result.Workset {
		result.Workset[i].Text = redact(result.Workset[i].Text)
	}
	if result.State != nil {
		state := RedactState(*result.State, redact)
		result.State = &state
	}
	return result
}

// RedactState returns a copy of state with every projected text field passed
// through redact, including history messages and skill descriptions.
func RedactState(state State, redact RedactFunc) State {
	state.Title = redact(state.Title)
	state.ModelAlias = redact(state.ModelAlias)
	state.ModelError = redact(state.ModelError)
	state.ProviderLabel = redact(state.ProviderLabel)
	state.Profile = redact(state.Profile)
	state.Workspace = redact(state.Workspace)
	for i := range state.History {
		state.History[i] = RedactMessage(state.History[i], redact)
	}
	for i := range state.Models {
		state.Models[i].Alias = redact(state.Models[i].Alias)
		state.Models[i].Provider = redact(state.Models[i].Provider)
		state.Models[i].Upstream = redact(state.Models[i].Upstream)
	}
	state.Skills = append([]SkillRef(nil), state.Skills...)
	for i := range state.Skills {
		state.Skills[i].Name = redact(state.Skills[i].Name)
		state.Skills[i].Description = redact(state.Skills[i].Description)
	}
	return state
}

// RedactMessage returns a copy of message with every projected text field
// passed through redact, including tool-use JSON inputs.
func RedactMessage(message llm.Message, redact RedactFunc) llm.Message {
	message.Blocks = append([]llm.Block(nil), message.Blocks...)
	for i := range message.Blocks {
		block := &message.Blocks[i]
		block.Text = redact(block.Text)
		block.Signature = redact(block.Signature)
		block.Data = redact(block.Data)
		if block.ToolUse != nil {
			toolUse := *block.ToolUse
			block.ToolUse = &toolUse
			block.ToolUse.ID = redact(block.ToolUse.ID)
			block.ToolUse.Name = redact(block.ToolUse.Name)
			block.ToolUse.Input = RedactJSON(block.ToolUse.Input, redact)
		}
		if block.ToolResult != nil {
			toolResult := *block.ToolResult
			block.ToolResult = &toolResult
			block.ToolResult.ToolUseID = redact(block.ToolResult.ToolUseID)
			block.ToolResult.Content = redact(block.ToolResult.Content)
		}
	}
	return message
}

// RedactJSON walks a JSON tool input and passes every string value through
// redact. Unparseable input is returned unchanged.
func RedactJSON(raw json.RawMessage, redact RedactFunc) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	var walk func(any) any
	walk = func(current any) any {
		switch typed := current.(type) {
		case string:
			return redact(typed)
		case []any:
			for i := range typed {
				typed[i] = walk(typed[i])
			}
		case map[string]any:
			for key, item := range typed {
				typed[key] = walk(item)
			}
		}
		return current
	}
	encoded, err := json.Marshal(walk(value))
	if err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return encoded
}
