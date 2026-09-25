// Package pair contains the transport and protocol seams for online Homer
// device pairing. The production tailcat adapter and the in-process test
// transport both implement the same one-shot session interface.
package pair

import (
	"context"
	"io"
	"time"
)

// PairTransport produces the one bidirectional session stream used by the
// pairing protocol. Production uses the tailcat CLI adapter; tests use an
// in-process transport.
type PairTransport interface {
	// Serve starts a one-shot server. The returned PairServer exposes the
	// ephemeral address before the peer connects.
	Serve(ctx context.Context) (PairServer, error)
	// Connect dials the peer's address and returns the session stream.
	Connect(ctx context.Context, addr string) (io.ReadWriteCloser, error)
}

// PairServer is the serve-side session handle.
type PairServer interface {
	// Addr is the one-shot tailcat address to exchange out of band.
	Addr() string
	// Accept blocks until the peer connects and returns the session stream.
	// One-shot: a second call returns an error.
	Accept(ctx context.Context) (io.ReadWriteCloser, error)
	// Close tears down the listener and any child process; idempotent.
	Close() error
}

// Deadline constants are centralized here so protocol orchestration can use
// stable production bounds while tests retain control through injected
// transports and contexts.
const (
	AddrFilePollInterval = 100 * time.Millisecond
	AddrFileTimeout      = 30 * time.Second
	AcceptDeadline       = 10 * time.Minute
	StepDeadline         = 120 * time.Second
)

// Protocol message and frame constants are declared with the transport seam
// so P0 consumers can compile against the frozen wire vocabulary. Frame
// encoding and validation are implemented in the P1 protocol wave.
const ProtocolVersion = 1

const (
	MaxFrameBytes       = 16 << 20
	MaxTotalBundleBytes = 64 << 20
)

const (
	FrameHello   uint8 = 1
	FrameDecline uint8 = 2
	FrameOffer   uint8 = 3
	FrameBlob    uint8 = 4
	FrameAck     uint8 = 5
	FrameAbort   uint8 = 6
)

// PeerHello is the join-side greeting.
type PeerHello struct {
	Version      int    `json:"version"`
	Hostname     string `json:"hostname"`
	Recipient    string `json:"recipient"`
	HomerVersion string `json:"homerVersion,omitempty"`
}

// Decline is sent when the serve-side confirmation gate refuses a peer.
type Decline struct {
	Reason string `json:"reason,omitempty"`
}

// OfferFile identifies one encrypted vault blob in an offer.
type OfferFile struct {
	Name string `json:"name"`
	Size int    `json:"size"`
}

// Offer is the serve-side encrypted-file manifest.
type Offer struct {
	Version int         `json:"version"`
	Files   []OfferFile `json:"files"`
}

// Ack is the join-side all-or-nothing result.
type Ack struct {
	OK        bool     `json:"ok"`
	Applied   []string `json:"applied,omitempty"`
	BackupDir string   `json:"backupDir,omitempty"`
	Errors    []string `json:"errors,omitempty"`
}

// Abort reports a protocol failure before either side applies partial state.
type Abort struct {
	Reason string `json:"reason,omitempty"`
}
