package stream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// EncodeFrame encodes a protocol frame as JSON and rejects encoded messages
// larger than maxFrame. Unknown frame types are permitted for forward
// compatibility.
func EncodeFrame(frame *Frame, maxFrame int) ([]byte, error) {
	if frame == nil {
		return nil, errors.New("stream: nil frame")
	}
	if maxFrame <= 0 {
		maxFrame = DefaultMaxFrame
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return nil, fmt.Errorf("stream: encode frame: %w", err)
	}
	if len(encoded) > maxFrame {
		return nil, ErrFrameTooLarge
	}
	return encoded, nil
}

// DecodeFrame decodes one JSON frame. Invalid JSON, missing frame type, empty
// messages and oversized messages are protocol errors. Unrecognized frame
// types and fields are intentionally accepted.
func DecodeFrame(data []byte, maxFrame int) (*Frame, error) {
	if maxFrame <= 0 {
		maxFrame = DefaultMaxFrame
	}
	if len(data) == 0 {
		return nil, &ProtocolError{Code: CloseProtocol, Msg: "empty frame"}
	}
	if len(data) > maxFrame {
		return nil, &ProtocolError{Code: CloseTooBig, Msg: "frame exceeds max size"}
	}
	if !utf8.Valid(data) {
		return nil, &ProtocolError{Code: CloseProtocol, Msg: "frame is not valid UTF-8"}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var frame Frame
	if err := decoder.Decode(&frame); err != nil {
		return nil, &ProtocolError{Code: CloseProtocol, Msg: "invalid JSON: " + err.Error()}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, &ProtocolError{Code: CloseProtocol, Msg: "multiple JSON values in frame"}
	} else if !errors.Is(err, io.EOF) {
		return nil, &ProtocolError{Code: CloseProtocol, Msg: "invalid trailing JSON: " + err.Error()}
	}
	if frame.T == "" {
		return nil, &ProtocolError{Code: CloseProtocol, Msg: "frame type is required"}
	}
	return &frame, nil
}
