// Package chatwire implements Waffle's bounded, versioned local chat protocol.
package chatwire

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// ProtocolVersion is the only protocol version this package accepts.
	ProtocolVersion = 1
	// MaxFrameBytes is the maximum encoded JSON size of one physical NDJSON line.
	MaxFrameBytes = 1 << 20
)

const (
	TypeOpen    = "open"
	TypeTurn    = "turn"
	TypeCommand = "command"
	TypeCancel  = "cancel"
	TypeClose   = "close"

	TypeReady         = "ready"
	TypeState         = "state"
	TypeTextDelta     = "text_delta"
	TypeToolStarted   = "tool_started"
	TypeToolFinished  = "tool_finished"
	TypeCommandResult = "command_result"
	TypeNotice        = "notice"
	TypeTurnDone      = "turn_done"
	TypeError         = "error"
	TypeGoodbye       = "goodbye"
)

var (
	ErrFrameTooLarge   = errors.New("chat wire frame exceeds maximum size")
	ErrProtocolVersion = errors.New("unsupported chat wire protocol version")
	ErrFrameType       = errors.New("invalid chat wire frame type")
	ErrMalformedFrame  = errors.New("malformed chat wire frame")
)

// Frame is the protocol's versioned NDJSON envelope.
type Frame struct {
	Version int             `json:"version"`
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ErrorPayload is the stable, redacted error surface sent to clients.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// TurnPayload carries one model input turn.
type TurnPayload struct {
	Text string `json:"text"`
}

func newFrame(frameType, id string, payload any) (Frame, error) {
	frame := Frame{Version: ProtocolVersion, Type: frameType, ID: id}
	if payload == nil {
		return frame, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Frame{}, fmt.Errorf("marshal %s payload: %w", frameType, err)
	}
	frame.Payload = raw
	return frame, nil
}

func decodePayload(frame Frame, target any) error {
	if len(frame.Payload) == 0 {
		return fmt.Errorf("%w: %s payload is missing", ErrMalformedFrame, frame.Type)
	}
	if err := json.Unmarshal(frame.Payload, target); err != nil {
		return fmt.Errorf("%w: decode %s payload", ErrMalformedFrame, frame.Type)
	}
	return nil
}
