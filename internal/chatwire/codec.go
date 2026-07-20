package chatwire

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var clientFrameTypes = map[string]struct{}{
	TypeOpen: {}, TypeTurn: {}, TypeCommand: {}, TypeCancel: {}, TypeClose: {},
}

var serverFrameTypes = map[string]struct{}{
	TypeReady: {}, TypeState: {}, TypeTextDelta: {}, TypeToolStarted: {},
	TypeToolFinished: {}, TypeCommandResult: {}, TypeNotice: {}, TypeTurnDone: {},
	TypeError: {}, TypeGoodbye: {},
}

// Codec reads and writes one direction-checked NDJSON protocol stream.
type Codec struct {
	reader   *bufio.Reader
	writer   *bufio.Writer
	inbound  map[string]struct{}
	outbound map[string]struct{}
}

// NewClientCodec returns a codec whose outbound frames are client requests
// and whose inbound frames are server responses.
func NewClientCodec(reader io.Reader, writer io.Writer) *Codec {
	return newCodec(reader, writer, serverFrameTypes, clientFrameTypes)
}

// NewServerCodec returns a codec whose outbound frames are server responses
// and whose inbound frames are client requests.
func NewServerCodec(reader io.Reader, writer io.Writer) *Codec {
	return newCodec(reader, writer, clientFrameTypes, serverFrameTypes)
}

func newCodec(reader io.Reader, writer io.Writer, inbound, outbound map[string]struct{}) *Codec {
	codec := &Codec{inbound: inbound, outbound: outbound}
	if reader != nil {
		codec.reader = bufio.NewReaderSize(reader, MaxFrameBytes+1)
	}
	if writer != nil {
		codec.writer = bufio.NewWriter(writer)
	}
	return codec
}

// Decode reads and validates one physical NDJSON line.
func (c *Codec) Decode() (Frame, error) {
	if c == nil || c.reader == nil {
		return Frame{}, fmt.Errorf("%w: codec has no reader", ErrMalformedFrame)
	}
	line, err := c.readLine()
	if err != nil {
		return Frame{}, err
	}
	var frame Frame
	if err := json.Unmarshal(line, &frame); err != nil {
		return Frame{}, fmt.Errorf("%w: invalid JSON", ErrMalformedFrame)
	}
	if err := validateFrame(frame, c.inbound); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

// Encode validates and writes one physical NDJSON line.
func (c *Codec) Encode(frame Frame) error {
	if c == nil || c.writer == nil {
		return fmt.Errorf("%w: codec has no writer", ErrMalformedFrame)
	}
	if err := validateFrame(frame, c.outbound); err != nil {
		return err
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("%w: encode JSON", ErrMalformedFrame)
	}
	if len(encoded) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	if _, err := c.writer.Write(encoded); err != nil {
		return fmt.Errorf("write chat wire frame: %w", err)
	}
	if err := c.writer.WriteByte('\n'); err != nil {
		return fmt.Errorf("terminate chat wire frame: %w", err)
	}
	if err := c.writer.Flush(); err != nil {
		return fmt.Errorf("flush chat wire frame: %w", err)
	}
	return nil
}

func (c *Codec) readLine() ([]byte, error) {
	line, err := c.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, ErrFrameTooLarge
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read chat wire frame: %w", err)
	}
	if errors.Is(err, io.EOF) && len(line) == 0 {
		return nil, io.EOF
	}
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	if len(line) == 0 {
		return nil, fmt.Errorf("%w: empty line", ErrMalformedFrame)
	}
	return line, nil
}

func validateFrame(frame Frame, allowed map[string]struct{}) error {
	if frame.Version != ProtocolVersion {
		return &ProtocolVersionError{Got: frame.Version, Want: ProtocolVersion, ID: frame.ID}
	}
	if _, ok := allowed[frame.Type]; !ok {
		return fmt.Errorf("%w: %q", ErrFrameType, frame.Type)
	}
	return nil
}
