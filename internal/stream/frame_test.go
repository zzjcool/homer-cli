package stream

import (
	"errors"
	"testing"
)

func TestFrameCodec(t *testing.T) {
	frame := &Frame{T: TReq, ID: "request-1", M: "status", P: []byte(`{"hello":"world"}`), DL: 5000}
	encoded, err := EncodeFrame(frame, 1024)
	if err != nil {
		t.Fatalf("EncodeFrame() error = %v", err)
	}
	got, err := DecodeFrame(encoded, 1024)
	if err != nil {
		t.Fatalf("DecodeFrame() error = %v", err)
	}
	if got.T != frame.T || got.ID != frame.ID || got.M != frame.M || string(got.P) != string(frame.P) || got.DL != frame.DL {
		t.Fatalf("round trip = %#v, want %#v", got, frame)
	}

	got, err = DecodeFrame([]byte(`{"t":"future","extra":true}`), 1024)
	if err != nil {
		t.Fatalf("forward-compatible DecodeFrame() error = %v", err)
	}
	if got.T != "future" || got.ID != "" || got.P != nil {
		t.Fatalf("unknown frame decode = %#v", got)
	}

	encoded, err = EncodeFrame(&Frame{T: TPing}, 1024)
	if err != nil {
		t.Fatalf("EncodeFrame(default fields) error = %v", err)
	}
	if string(encoded) != `{"t":"ping"}` {
		t.Fatalf("default-field encoding = %s", encoded)
	}
}

func TestFrameDecodeInvalid(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		max  int
		code CloseCode
	}{
		{name: "malformed", data: []byte(`{"t":`), max: 10, code: CloseProtocol},
		{name: "missing type", data: []byte(`{"id":"x"}`), max: 100, code: CloseProtocol},
		{name: "empty", data: nil, max: 100, code: CloseProtocol},
		{name: "oversized", data: []byte(`{"t":"ping"}`), max: 5, code: CloseTooBig},
		{name: "trailing value", data: []byte(`{"t":"ping"}{}`), max: 100, code: CloseProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeFrame(test.data, test.max)
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("DecodeFrame() error = %T %v, want *ProtocolError", err, err)
			}
			if protocolErr.Code != test.code {
				t.Fatalf("ProtocolError.Code = %d, want %d", protocolErr.Code, test.code)
			}
		})
	}
}

func TestEncodeFrameTooLarge(t *testing.T) {
	if _, err := EncodeFrame(&Frame{T: TReq, P: []byte(`"1234567890"`)}, 5); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("EncodeFrame() error = %v, want ErrFrameTooLarge", err)
	}
}
