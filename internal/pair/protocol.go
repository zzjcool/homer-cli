package pair

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/zzjcool/homer-cli/internal/agecrypto"
)

// FrameError is reserved for a peer-side protocol error. P1 uses FrameAbort
// for all-or-nothing failures, but accepting a distinct error frame keeps the
// wire reader forward-compatible with transports that report one explicitly.
const FrameError uint8 = 7

// ErrFrameTooLarge guards against a malicious peer forcing an oversized
// allocation.
var ErrFrameTooLarge = errors.New("pair frame exceeds size limit")

var errUnknownFrameType = errors.New("pair frame has unknown type")

func knownFrameType(frameType uint8) bool {
	switch frameType {
	case FrameHello, FrameDecline, FrameOffer, FrameBlob, FrameAck, FrameAbort, FrameError:
		return true
	default:
		return false
	}
}

// WriteFrame writes one length-prefixed frame. The four-byte length is the
// payload length; the frame type is the byte immediately following it. A
// single write keeps the frame boundary intact for transports such as
// net.Pipe, while short writes are still reported as io.ErrShortWrite.
func WriteFrame(w io.Writer, frameType uint8, payload []byte) error {
	if !knownFrameType(frameType) {
		return fmt.Errorf("%w: %d", errUnknownFrameType, frameType)
	}
	if len(payload) > MaxFrameBytes {
		return ErrFrameTooLarge
	}

	frame := make([]byte, 4+1+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	frame[4] = frameType
	copy(frame[5:], payload)
	written, err := w.Write(frame)
	if err != nil {
		return err
	}
	if written != len(frame) {
		return io.ErrShortWrite
	}
	return nil
}

// ReadFrame reads one frame with io.ReadFull semantics. The declared length
// is checked before allocating the payload, so a hostile peer cannot force an
// unbounded allocation. EOF and short reads retain their original cause via
// wrapping, which lets callers use errors.Is with io.EOF or io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader, maxBytes int) (frameType uint8, payload []byte, err error) {
	var lengthBytes [4]byte
	if _, err := io.ReadFull(r, lengthBytes[:]); err != nil {
		return 0, nil, fmt.Errorf("read pair frame length: %w", err)
	}
	declared := binary.BigEndian.Uint32(lengthBytes[:])
	if maxBytes < 0 || uint64(declared) > uint64(maxBytes) {
		return 0, nil, ErrFrameTooLarge
	}

	var typeByte [1]byte
	if _, err := io.ReadFull(r, typeByte[:]); err != nil {
		return 0, nil, fmt.Errorf("read pair frame type: %w", err)
	}
	frameType = typeByte[0]
	if !knownFrameType(frameType) {
		return 0, nil, fmt.Errorf("%w: %d", errUnknownFrameType, frameType)
	}

	payload = make([]byte, int(declared))
	if declared == 0 {
		return frameType, payload, nil
	}
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("read pair frame payload: %w", err)
	}
	return frameType, payload, nil
}

// ValidateHello checks the peer greeting before any public-key material is
// persisted or any encrypted bundle is sent. Hostnames are deliberately
// restricted to printable ASCII so an untrusted peer cannot inject terminal
// control sequences into the confirmation prompt.
func ValidateHello(hello PeerHello) error {
	if hello.Version != ProtocolVersion {
		return fmt.Errorf("unsupported pair protocol version")
	}
	if !agecrypto.RecipientValid(hello.Recipient) {
		return fmt.Errorf("invalid pair recipient")
	}
	if len(hello.Hostname) == 0 || len(hello.Hostname) > 63 {
		return fmt.Errorf("invalid pair hostname")
	}
	for index := 0; index < len(hello.Hostname); index++ {
		if hello.Hostname[index] < 0x20 || hello.Hostname[index] > 0x7e {
			return fmt.Errorf("invalid pair hostname")
		}
	}
	return nil
}

// PeerDisplay renders only the hostname and an abbreviated recipient. The
// complete recipient remains in the in-memory report and config update, but
// is never echoed in the confirmation prompt.
func PeerDisplay(hello PeerHello) string {
	fingerprint := "invalid recipient"
	if len(hello.Recipient) >= 16 {
		fingerprint = hello.Recipient[:12] + "…" + hello.Recipient[len(hello.Recipient)-4:]
	}
	return hello.Hostname + "（" + fingerprint + "，homer pair）"
}

func decodeObject(payload []byte, target any) error {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("pair message must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode pair message: %w", err)
	}
	// Decoder.Decode accepts a second JSON value unless it is explicitly
	// checked. Reject trailing non-whitespace bytes so one frame cannot smuggle
	// two protocol messages into a single JSON payload.
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("pair message has trailing JSON")
		}
		return fmt.Errorf("decode pair message trailer: %w", err)
	}
	return nil
}

// DecodeHello decodes a JSON peer greeting. Semantic validation is kept
// separate so callers can choose whether to report a malformed message or an
// unsupported peer after decoding.
func DecodeHello(payload []byte) (PeerHello, error) {
	var hello PeerHello
	if err := decodeObject(payload, &hello); err != nil {
		return PeerHello{}, err
	}
	return hello, nil
}

// DecodeOffer decodes a JSON encrypted-file manifest.
func DecodeOffer(payload []byte) (Offer, error) {
	var offer Offer
	if err := decodeObject(payload, &offer); err != nil {
		return Offer{}, err
	}
	return offer, nil
}

// DecodeAck decodes the join-side result message.
func DecodeAck(payload []byte) (Ack, error) {
	var ack Ack
	if err := decodeObject(payload, &ack); err != nil {
		return Ack{}, err
	}
	return ack, nil
}
