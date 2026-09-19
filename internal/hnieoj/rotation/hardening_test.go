package rotation

import (
	"testing"

	"github.com/criyle/go-judge/internal/hnieoj/identity"
)

// TestValidateRotationResponseRejectsForeignState 覆盖 R6：
// 服务端返回的 rotationId/keyId/公钥不属于本地这一轮时，绝不能据此激活本地 key。
func TestValidateRotationResponseRejectsForeignState(t *testing.T) {
	pending := &identity.PendingRotation{
		RotationID:   "rotation-1",
		NewKeyID:     "key-2",
		NewPublicKey: "pub-2",
		ConfirmNonce: "nonce-1",
	}
	if err := validateRotationResponse(pending, &QueryResponse{
		RotationID: "other", KeyID: "key-2", NewPublicKey: "pub-2", Status: "ACTIVE",
	}); err == nil {
		t.Fatal("foreign rotationId accepted")
	}
	if err := validateRotationResponse(pending, &QueryResponse{
		RotationID: "rotation-1", KeyID: "key-9", NewPublicKey: "pub-2", Status: "ACTIVE",
	}); err == nil {
		t.Fatal("foreign keyId accepted")
	}
	if err := validateRotationResponse(pending, &QueryResponse{
		RotationID: "rotation-1", KeyID: "key-2", NewPublicKey: "pub-9", Status: "ACTIVE",
	}); err == nil {
		t.Fatal("foreign newPublicKey accepted")
	}
	if err := validateRotationResponse(pending, &QueryResponse{
		RotationID: "rotation-1", KeyID: "key-2", NewPublicKey: "pub-2", Status: "ACTIVE",
	}); err != nil {
		t.Fatalf("matching rotation rejected: %v", err)
	}
}

// TestValidateRotationResponseRejectsUnknownStatus 保证 status 只能是合同登记值。
func TestValidateRotationResponseRejectsUnknownStatus(t *testing.T) {
	pending := &identity.PendingRotation{RotationID: "r", NewKeyID: "k", NewPublicKey: "p"}
	if err := validateRotationResponse(pending, &QueryResponse{RotationID: "r", KeyID: "k", NewPublicKey: "p", Status: "WAT"}); err == nil {
		t.Fatal("unknown rotation status accepted")
	}
}
