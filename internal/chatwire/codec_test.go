package chatwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
)

func TestChatEventJSONRoundTripsAttachedSkills(t *testing.T) {
	t.Parallel()
	want := chat.State{
		SessionID: "01SKILLS",
		Skills: []chat.SkillRef{
			{Name: "reviewer", Description: "review changes", Attached: true},
			{Name: "removed", Attached: true, Missing: true},
		},
	}
	tests := []struct {
		name      string
		frameType string
		payload   any
		decode    func(Frame) chat.State
	}{
		{
			name: "ready", frameType: TypeReady, payload: want,
			decode: func(frame Frame) chat.State {
				var state chat.State
				if err := decodePayload(frame, &state); err != nil {
					t.Fatal(err)
				}
				return state
			},
		},
		{
			name: "state_event", frameType: TypeState,
			payload: chat.Event{Kind: chat.EventState, State: &want},
			decode: func(frame Frame) chat.State {
				var event chat.Event
				if err := decodePayload(frame, &event); err != nil {
					t.Fatal(err)
				}
				if event.State == nil {
					t.Fatal("decoded event state is nil")
				}
				return *event.State
			},
		},
		{
			name: "command_result", frameType: TypeCommandResult,
			payload: chat.Result{Title: "Skills", State: &want},
			decode: func(frame Frame) chat.State {
				var result chat.Result
				if err := decodePayload(frame, &result); err != nil {
					t.Fatal(err)
				}
				if result.State == nil {
					t.Fatal("decoded result state is nil")
				}
				return *result.State
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			frame, err := newFrame(tt.frameType, "skills-1", tt.payload)
			if err != nil {
				t.Fatal(err)
			}
			var wire bytes.Buffer
			if err := NewServerCodec(nil, &wire).Encode(frame); err != nil {
				t.Fatal(err)
			}
			decoded, err := NewClientCodec(&wire, nil).Decode()
			if err != nil {
				t.Fatal(err)
			}
			if got := tt.decode(decoded); !reflect.DeepEqual(got.Skills, want.Skills) {
				t.Fatalf("skills = %+v, want %+v", got.Skills, want.Skills)
			}
		})
	}
}

func TestCodecRoundTripsAllowedFrames(t *testing.T) {
	t.Parallel()

	clientPayloads := map[string]any{
		TypeOpen:    chat.OpenOptions{Continue: true, SessionID: "01TEST", Profile: "host", Capabilities: []string{"stream"}},
		TypeTurn:    TurnPayload{Text: "hello\nworld"},
		TypeCommand: chat.ParsedCommand{Name: chat.CommandStatus, Args: ""},
		TypeCancel:  nil,
		TypeClose:   nil,
	}
	serverPayloads := map[string]any{
		TypeReady:         chat.State{SessionID: "01TEST", ModelAlias: "gpt"},
		TypeState:         chat.Event{Kind: chat.EventState, State: &chat.State{SessionID: "01NEXT"}},
		TypeTextDelta:     chat.Event{Kind: chat.EventTextDelta, Text: "answer"},
		TypeToolStarted:   chat.Event{Kind: chat.EventToolStarted, ToolName: "read"},
		TypeToolFinished:  chat.Event{Kind: chat.EventToolFinished, ToolName: "read", ByteCount: 42},
		TypeCommandResult: chat.Result{Title: "status", Text: "ready"},
		TypeNotice:        chat.Event{Kind: chat.EventNotice, Text: "notice"},
		TypeTurnDone:      chat.Event{Kind: chat.EventTurnDone},
		TypeError:         ErrorPayload{Code: "turn_failed", Message: "chat turn failed"},
		TypeGoodbye:       nil,
	}

	for name, tc := range map[string]struct {
		payloads map[string]any
		encoder  func(*bytes.Buffer) *Codec
		decoder  func(*bytes.Buffer) *Codec
	}{
		"client_to_server": {clientPayloads, func(w *bytes.Buffer) *Codec { return NewClientCodec(nil, w) }, func(r *bytes.Buffer) *Codec { return NewServerCodec(r, nil) }},
		"server_to_client": {serverPayloads, func(w *bytes.Buffer) *Codec { return NewServerCodec(nil, w) }, func(r *bytes.Buffer) *Codec { return NewClientCodec(r, nil) }},
	} {
		name, tc := name, tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for frameType, payload := range tc.payloads {
				frameType, payload := frameType, payload
				t.Run(frameType, func(t *testing.T) {
					t.Parallel()
					frame, err := newFrame(frameType, "request-1", payload)
					if err != nil {
						t.Fatal(err)
					}
					var wire bytes.Buffer
					if err := tc.encoder(&wire).Encode(frame); err != nil {
						t.Fatalf("Encode: %v", err)
					}
					got, err := tc.decoder(&wire).Decode()
					if err != nil {
						t.Fatalf("Decode: %v", err)
					}
					if got.Version != ProtocolVersion || got.Type != frameType || got.ID != "request-1" {
						t.Fatalf("frame = %+v", got)
					}
					if !jsonEqual(t, got.Payload, frame.Payload) {
						t.Fatalf("payload = %s, want %s", got.Payload, frame.Payload)
					}
				})
			}
		})
	}
}

func TestCodecRejectsInvalidInboundFrames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want error
	}{
		{name: "version_zero", line: `{"version":0,"type":"open"}` + "\n", want: ErrProtocolVersion},
		{name: "wrong_version", line: `{"version":2,"type":"open"}` + "\n", want: ErrProtocolVersion},
		{name: "unknown_type", line: `{"version":1,"type":"future"}` + "\n", want: ErrFrameType},
		{name: "wrong_direction", line: `{"version":1,"type":"ready"}` + "\n", want: ErrFrameType},
		{name: "malformed_json", line: `{"version":1,"type":"open"` + "\n", want: ErrMalformedFrame},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewServerCodec(strings.NewReader(tt.line), nil).Decode()
			if !errors.Is(err, tt.want) {
				t.Fatalf("Decode error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCodecProtocolVersionErrorPreservesVersions(t *testing.T) {
	t.Parallel()
	_, err := NewServerCodec(strings.NewReader(`{"version":7,"type":"open"}`+"\n"), nil).Decode()
	var mismatch *ProtocolVersionError
	if !errors.As(err, &mismatch) {
		t.Fatalf("Decode error = %#v, want ProtocolVersionError", err)
	}
	if mismatch.Got != 7 || mismatch.Want != ProtocolVersion {
		t.Fatalf("mismatch = %+v", mismatch)
	}
}

func TestCodecRejectsInvalidOutboundFrames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		frame Frame
		want  error
	}{
		{name: "version_zero", frame: Frame{Type: TypeOpen}, want: ErrProtocolVersion},
		{name: "wrong_version", frame: Frame{Version: 2, Type: TypeOpen}, want: ErrProtocolVersion},
		{name: "unknown_type", frame: Frame{Version: ProtocolVersion, Type: "future"}, want: ErrFrameType},
		{name: "wrong_direction", frame: Frame{Version: ProtocolVersion, Type: TypeReady}, want: ErrFrameType},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var wire bytes.Buffer
			err := NewClientCodec(nil, &wire).Encode(tt.frame)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Encode error = %v, want %v", err, tt.want)
			}
			if wire.Len() != 0 {
				t.Fatalf("invalid frame wrote %d bytes", wire.Len())
			}
		})
	}
}

func TestCodecRejectsOverlongPhysicalLineBeforeJSON(t *testing.T) {
	t.Parallel()

	line := strings.Repeat("not-json", MaxFrameBytes/len("not-json")+2) + "\n"
	_, err := NewServerCodec(strings.NewReader(line), nil).Decode()
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Decode error = %v, want %v", err, ErrFrameTooLarge)
	}
}

func TestCodecRejectsStreamingOverlongLineWithoutWaitingForNewline(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	go func() {
		_, _ = writer.Write(bytes.Repeat([]byte{'x'}, MaxFrameBytes+2))
	}()
	result := make(chan error, 1)
	go func() {
		_, err := NewServerCodec(reader, nil).Decode()
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("Decode error = %v, want %v", err, ErrFrameTooLarge)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Decode waited for a newline after exceeding MaxFrameBytes")
	}
}

func TestCodecRejectsExactFirstInvalidLengthWithoutWaitingForAnotherByte(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	go func() {
		_, _ = writer.Write(bytes.Repeat([]byte{'x'}, MaxFrameBytes+1))
	}()
	result := make(chan error, 1)
	go func() {
		_, err := NewServerCodec(reader, nil).Decode()
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("Decode error = %v, want %v", err, ErrFrameTooLarge)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Decode waited after receiving MaxFrameBytes+1 bytes")
	}
}

func TestCodecRejectsOversizeEncodedFrame(t *testing.T) {
	t.Parallel()

	frame, err := newFrame(TypeTurn, "request-1", TurnPayload{Text: strings.Repeat("x", MaxFrameBytes)})
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	err = NewClientCodec(nil, &wire).Encode(frame)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Encode error = %v, want %v", err, ErrFrameTooLarge)
	}
	if wire.Len() != 0 {
		t.Fatalf("oversize frame wrote %d bytes", wire.Len())
	}
}

func jsonEqual(t *testing.T, left, right []byte) bool {
	t.Helper()
	var leftValue, rightValue any
	if len(left) > 0 {
		if err := json.Unmarshal(left, &leftValue); err != nil {
			t.Fatalf("unmarshal left: %v", err)
		}
	}
	if len(right) > 0 {
		if err := json.Unmarshal(right, &rightValue); err != nil {
			t.Fatalf("unmarshal right: %v", err)
		}
	}
	return jsonValuesEqual(leftValue, rightValue)
}

func jsonValuesEqual(left, right any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}
