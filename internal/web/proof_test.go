package web

import (
	"testing"
	"time"
)

// Compile-time proof that EnrollmentService carries ValidCode (the L58
// "undefined" advisory is stale — this file cannot compile otherwise).
func TestEnrollmentServiceHasValidCode(t *testing.T) {
	var service EnrollmentService = proofValidCodeService{}
	if !service.ValidCode("hr_x") {
		t.Fatal("ValidCode must exist and be callable on EnrollmentService")
	}
}

type proofValidCodeService struct{}

func (proofValidCodeService) Mint(ttl time.Duration) (string, error) { return "", nil }
func (proofValidCodeService) Revoke(agentID string) bool             { return false }
func (proofValidCodeService) ValidCode(code string) bool             { return code != "" }
