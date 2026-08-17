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
	event.Artifacts = RedactArtifacts(event.Artifacts, redact)
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
		result.Models[i].Description = redact(result.Models[i].Description)
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
		state.Models[i].Description = redact(state.Models[i].Description)
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
		if block.Source != nil {
			// Redact the URL reference; base64 payloads are binary data and
			// are left untouched.
			source := *block.Source
			block.Source = &source
			block.Source.URL = redact(block.Source.URL)
		}
		if len(block.Citations) > 0 {
			block.Citations = append([]llm.Citation(nil), block.Citations...)
			for i := range block.Citations {
				block.Citations[i].ID = redact(block.Citations[i].ID)
				block.Citations[i].Label = redact(block.Citations[i].Label)
				block.Citations[i].URL = redact(block.Citations[i].URL)
				block.Citations[i].Resource = redact(block.Citations[i].Resource)
				block.Citations[i].Snippet = redact(block.Citations[i].Snippet)
				block.Citations[i].Provenance = redact(block.Citations[i].Provenance)
			}
		}
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
			// Mixed-content tool results: redact the text parts so a secret
			// inside a block-carrying result cannot leak through projection.
			block.ToolResult.Blocks = RedactBlocks(block.ToolResult.Blocks, redact)
		}
		if block.Artifact != nil {
			ref := *block.Artifact
			block.Artifact = &ref
			block.Artifact.Name = redact(block.Artifact.Name)
			block.Artifact.MediaType = redact(block.Artifact.MediaType)
			block.Artifact.Digest = redact(block.Artifact.Digest)
		}
	}
	return message
}

// RedactArtifacts returns a copy of artifact projections with every display
// field passed through redact (#480). The opaque ID is a server-assigned
// identifier and is left untouched so the client can still address it.
func RedactArtifacts(artifacts []Artifact, redact RedactFunc) []Artifact {
	out := append([]Artifact(nil), artifacts...)
	for i := range out {
		out[i].Name = redact(out[i].Name)
		out[i].MediaType = redact(out[i].MediaType)
		out[i].Digest = redact(out[i].Digest)
		out[i].ToolName = redact(out[i].ToolName)
	}
	return out
}

// RedactBlocks returns a copy of blocks with every projected text field
// (text block bodies and media URL references) passed through redact.
func RedactBlocks(blocks []llm.Block, redact RedactFunc) []llm.Block {
	out := make([]llm.Block, len(blocks))
	copy(out, blocks)
	for i := range out {
		out[i].Text = redact(out[i].Text)
		if out[i].Source != nil {
			source := *out[i].Source
			out[i].Source = &source
			out[i].Source.URL = redact(out[i].Source.URL)
		}
		if len(out[i].Citations) > 0 {
			out[i].Citations = append([]llm.Citation(nil), out[i].Citations...)
			for j := range out[i].Citations {
				out[i].Citations[j].ID = redact(out[i].Citations[j].ID)
				out[i].Citations[j].Label = redact(out[i].Citations[j].Label)
				out[i].Citations[j].URL = redact(out[i].Citations[j].URL)
				out[i].Citations[j].Resource = redact(out[i].Citations[j].Resource)
				out[i].Citations[j].Snippet = redact(out[i].Citations[j].Snippet)
				out[i].Citations[j].Provenance = redact(out[i].Citations[j].Provenance)
			}
		}
	}
	return out
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
