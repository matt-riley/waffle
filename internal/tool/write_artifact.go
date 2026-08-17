package tool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/matt-riley/waffle/internal/artifact"
	"github.com/matt-riley/waffle/internal/llm"
)

// WriteArtifact explicitly declares a file intentionally produced inside the
// authorized session (#480). The payload is persisted to the artifact
// registry keyed by an opaque ID; the returned block carries safe metadata
// only — never a host path. The artifact is then available to the Desk for
// preview and owner-authorized download.
type WriteArtifact struct {
	Store *artifact.Store
	// SessionID resolves the active session from the tool context. The
	// builder wires it to agent.SessionID; without it the tool fails closed
	// (keeps tool from importing agent/session, which would cycle).
	SessionID func(context.Context) string
}

// Def describes the tool for the model. Content is capped at the artifact
// size limit by the artifact store; the schema documents the safe types.
func (w *WriteArtifact) Def() llm.Tool {
	return llm.Tool{
		Name:        "write_artifact",
		Description: "Declare a file intentionally produced for the operator as a session artifact. Use this for final deliverables (reports, code files, images, PDFs) the operator should preview or download. Returns the opaque artifact ID; the Desk shows a card with preview and download.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Safe file name without path separators, e.g. summary.md"},
				"media_type": {"type": "string", "description": "Media type of the payload, e.g. text/markdown, image/png, application/pdf"},
				"content": {"type": "string", "description": "The file content (text or base64 for binary payloads)"}
			},
			"required": ["name", "media_type", "content"]
		}`),
	}
}

// Run persists the artifact and returns the opaque ID plus safe metadata so
// the model's tool evidence matches the artifact it produced (#480).
func (w *WriteArtifact) Run(ctx context.Context, input json.RawMessage) (string, error) {
	block, err := w.run(ctx, input)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("artifact created: %s (%s, %d bytes) — artifact id %s", block.Artifact.Name, block.Artifact.MediaType, block.Artifact.Size, block.Artifact.ID), nil
}

// RunBlocks persists the artifact and returns it as a BlockArtifact so the
// declaration persists in the turn at the producing tool's transcript
// position.
func (w *WriteArtifact) RunBlocks(ctx context.Context, input json.RawMessage) (string, []llm.Block, error) {
	block, err := w.run(ctx, input)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("artifact created: %s (%s, %d bytes) — artifact id %s", block.Artifact.Name, block.Artifact.MediaType, block.Artifact.Size, block.Artifact.ID), []llm.Block{block}, nil
}

func (w *WriteArtifact) run(ctx context.Context, input json.RawMessage) (llm.Block, error) {
	if w == nil || w.Store == nil {
		return llm.Block{}, fmt.Errorf("artifact store unavailable")
	}
	sessionID := ""
	if w.SessionID != nil {
		sessionID = w.SessionID(ctx)
	}
	if sessionID == "" {
		return llm.Block{}, fmt.Errorf("no active session for artifact")
	}
	var req struct {
		Name      string `json:"name"`
		MediaType string `json:"media_type"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return llm.Block{}, fmt.Errorf("write_artifact: %w", err)
	}
	payload := []byte(req.Content)
	if strings.EqualFold(req.MediaType, "image/png") || strings.EqualFold(req.MediaType, "image/jpeg") ||
		strings.EqualFold(req.MediaType, "image/gif") || strings.EqualFold(req.MediaType, "image/webp") ||
		strings.EqualFold(req.MediaType, "application/pdf") {
		// Binary payloads arrive base64-encoded.
		decoded, err := base64.StdEncoding.DecodeString(req.Content)
		if err != nil {
			return llm.Block{}, fmt.Errorf("write_artifact: base64 content: %w", err)
		}
		payload = decoded
	}
	stored, err := w.Store.Write(ctx, sessionID, "write_artifact", req.Name, req.MediaType, payload)
	if err != nil {
		return llm.Block{}, err
	}
	ref := stored.Ref()
	return llm.Block{Type: llm.BlockArtifact, Artifact: &ref}, nil
}
