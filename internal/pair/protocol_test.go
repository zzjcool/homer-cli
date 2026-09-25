package pair

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/zzjcool/homer-cli/internal/agecrypto"
)

type oneByteReader struct{ data []byte }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

type shortWriter struct {
	calls int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	w.calls++
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

func TestFrameRoundTripAndLengthPrefix(t *testing.T) {
	payload := []byte(`{"hostname":"machine","recipient":"age1..."}`)
	var encoded bytes.Buffer
	if err := WriteFrame(&encoded, FrameHello, payload); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint32(encoded.Bytes()[:4]); got != uint32(len(payload)) {
		t.Fatalf("declared payload length = %d, want %d", got, len(payload))
	}
	if encoded.Bytes()[4] != FrameHello {
		t.Fatalf("frame type = %d, want %d", encoded.Bytes()[4], FrameHello)
	}
	frameType, got, err := ReadFrame(bytes.NewReader(encoded.Bytes()), MaxFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	if frameType != FrameHello || !bytes.Equal(got, payload) {
		t.Fatalf("round trip = type %d payload %q", frameType, got)
	}
}

func TestFrameReadHandlesShortReadsAndEOF(t *testing.T) {
	var encoded bytes.Buffer
	if err := WriteFrame(&encoded, FrameBlob, []byte("ciphertext")); err != nil {
		t.Fatal(err)
	}
	frameType, payload, err := ReadFrame(&oneByteReader{data: encoded.Bytes()}, MaxFrameBytes)
	if err != nil || frameType != FrameBlob || string(payload) != "ciphertext" {
		t.Fatalf("short-read round trip = %d %q %v", frameType, payload, err)
	}

	for _, input := range [][]byte{nil, []byte{0, 0, 0}, append([]byte{0, 0, 0, 4, FrameBlob}, []byte("x")...)} {
		_, _, err := ReadFrame(bytes.NewReader(input), MaxFrameBytes)
		if err == nil {
			t.Fatalf("ReadFrame(%x) unexpectedly succeeded", input)
		}
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("ReadFrame(%x) error = %v, want EOF/UnexpectedEOF", input, err)
		}
	}
}

func TestFrameTooLargeRejectedBeforeAllocationOrWrite(t *testing.T) {
	writer := &shortWriter{}
	if err := WriteFrame(writer, FrameBlob, make([]byte, MaxFrameBytes+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("WriteFrame oversized error = %v", err)
	}
	if writer.calls != 0 {
		t.Fatalf("oversized WriteFrame called writer %d times", writer.calls)
	}

	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(MaxFrameBytes+1))
	if _, _, err := ReadFrame(bytes.NewReader(header[:]), MaxFrameBytes); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("ReadFrame oversized error = %v", err)
	}
}

func TestFrameTypeAndShortWriterValidation(t *testing.T) {
	if err := WriteFrame(io.Discard, 99, nil); err == nil {
		t.Fatal("unknown frame type was accepted")
	}
	if err := WriteFrame(&shortWriter{}, FrameAck, []byte("ack")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short writer error = %v, want io.ErrShortWrite", err)
	}
}

func TestValidateHelloBranchesAndDisplay(t *testing.T) {
	identity := agecrypto.GenerateIdentity()
	valid := PeerHello{Version: ProtocolVersion, Hostname: "machine-b", Recipient: identity.Recipient}
	if err := ValidateHello(valid); err != nil {
		t.Fatalf("valid hello rejected: %v", err)
	}
	display := PeerDisplay(valid)
	if !strings.Contains(display, "machine-b") || !strings.Contains(display, valid.Recipient[:12]) || !strings.Contains(display, valid.Recipient[len(valid.Recipient)-4:]) {
		t.Fatalf("peer display = %q", display)
	}
	if strings.Contains(display, valid.Recipient) {
		t.Fatal("peer display echoed the full recipient")
	}

	cases := []struct {
		name   string
		mutate func(*PeerHello)
	}{
		{name: "version", mutate: func(hello *PeerHello) { hello.Version++ }},
		{name: "recipient", mutate: func(hello *PeerHello) { hello.Recipient = "not-an-age-recipient" }},
		{name: "hostname length", mutate: func(hello *PeerHello) { hello.Hostname = strings.Repeat("a", 64) }},
		{name: "hostname control", mutate: func(hello *PeerHello) { hello.Hostname = "safe\x1b[31m" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := valid
			testCase.mutate(&candidate)
			if err := ValidateHello(candidate); err == nil {
				t.Fatal("invalid hello was accepted")
			}
		})
	}
}

func TestDecodeDeclineAndAbortHelpers(t *testing.T) {
	decline, err := DecodeDecline([]byte(`{"reason":"拒绝"}`))
	if err != nil || decline.Reason != "拒绝" {
		t.Fatalf("DecodeDecline = %#v, %v", decline, err)
	}
	abort, err := DecodeAbort([]byte(`{"reason":"前向兼容错误"}`))
	if err != nil || abort.Reason != "前向兼容错误" {
		t.Fatalf("DecodeAbort = %#v, %v", abort, err)
	}
	if _, err := DecodeAbort([]byte(`[]`)); err == nil {
		t.Fatal("DecodeAbort accepted an array")
	}
}

func TestDecodeHelpersRejectNonObjectsAndWrongTypes(t *testing.T) {
	if _, err := DecodeHello([]byte(`[]`)); err == nil {
		t.Fatal("array hello was accepted")
	}
	if _, err := DecodeOffer([]byte(`{"version":"one","files":[]}`)); err == nil {
		t.Fatal("wrong offer version type was accepted")
	}
	if _, err := DecodeAck([]byte(`{"ok":true,"applied":[7]}`)); err == nil {
		t.Fatal("wrong ack applied type was accepted")
	}
	if _, err := DecodeAck([]byte(`{"ok":true} {"ok":false}`)); err == nil {
		t.Fatal("trailing JSON was accepted")
	}
}
