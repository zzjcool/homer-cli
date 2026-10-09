package stream_test

import (
	"testing"

	"github.com/zzjcool/homer-cli/internal/stream"
)

func TestCloseCodeValues(t *testing.T) {
	tests := []struct {
		name string
		code stream.CloseCode
		want stream.CloseCode
	}{
		{"normal", stream.CloseNormal, 1000},
		{"going away", stream.CloseGoingAway, 1001},
		{"protocol", stream.CloseProtocol, 1002},
		{"unsupported", stream.CloseUnsupported, 1003},
		{"policy", stream.ClosePolicy, 1008},
		{"too big", stream.CloseTooBig, 1009},
		{"internal", stream.CloseInternal, 1011},
		{"heartbeat", stream.CloseHeartbeat, 4000},
		{"superseded", stream.CloseSuperseded, 4001},
		{"revoked", stream.CloseRevoked, 4401},
		{"removed", stream.CloseRemoved, 4403},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.code != test.want {
				t.Fatalf("%s close code = %d, want %d", test.name, test.code, test.want)
			}
		})
	}
}
