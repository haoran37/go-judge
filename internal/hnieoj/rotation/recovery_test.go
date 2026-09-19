package rotation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/httpsign"
	"github.com/criyle/go-judge/internal/hnieoj/identity"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
)

// pendingRecoveryFixture 构造 confirm 回包丢失、服务端已接受 pending 新 key 的场景。
func pendingRecoveryFixture(t *testing.T, active bool) (*identity.Store, *Manager, *atomic.Value) {
	t.Helper()
	store, err := identity.Open(filepath.Join(t.TempDir(), "identity.json"), "node", "formal")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatal(err)
	}
	pending, err := store.BeginRotation("rotation-1")
	if err != nil {
		t.Fatal(err)
	}
	// prepare 截止时间已过：本地不构成删除依据，恢复必须依赖服务端权威状态。
	if err := store.RecordRotationPrepared("rotation-1", "key-2", "nonce", time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	status := "EXPIRED"
	if active {
		status = "ACTIVE"
	}
	var seenKeyID atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKeyID.Store(r.Header.Get("X-Judge-Key-Id"))
		if r.Method == http.MethodPost {
			// confirm 已提交但响应丢失。
			http.Error(w, "confirm response lost", http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"code": 200, "msg": "ok", "data": map[string]any{
			"rotationId": "rotation-1", "keyId": "key-2", "status": status,
			"newPublicKey": pending.NewPublicKey, "confirmNonce": "nonce",
			"graceUntil": time.Now().Add(5 * time.Minute).UnixMilli(),
		}})
	}))
	t.Cleanup(srv.Close)
	// 服务端已接受新 key，AUTH_OK 会把 token 绑定到 pending keyId。
	cred := &auth.Credential{NodeID: "node-1", KeyID: "key-2", AccessToken: "token"}
	signer := httpsign.New(store, cred, "audience", srv.Client())
	manager := New(store, signer, cred, srv.URL, "audience", logging.NopLogger{}, Options{
		Enabled: true, Interval: time.Hour, Grace: time.Minute, ConfirmTimeout: time.Second,
	}, nil)
	return store, manager, &seenKeyID
}

// TestPendingKeySignsRecoveryQueryAndActivates 覆盖：confirm 回包丢失、服务端已把 pending
// 新 key 置为 ACTIVE 时，token 绑定的 pending key 必须能签恢复查询，并据权威 ACTIVE 完成激活。
func TestPendingKeySignsRecoveryQueryAndActivates(t *testing.T) {
	store, manager, seenKeyID := pendingRecoveryFixture(t, true)
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatalf("recover with pending key: %v", err)
	}
	if got, _ := seenKeyID.Load().(string); got != "key-2" {
		t.Fatalf("recovery query signed with key %q, want token-bound pending key-2", got)
	}
	pub := store.Public()
	if pub.KeyID != "key-2" {
		t.Fatalf("rotation not activated: %+v", pub)
	}
	if store.Pending() != nil {
		t.Fatal("pending rotation not cleared after authoritative ACTIVE")
	}
}

// TestAuthoritativeExpiredDiscardsPendingAfterLocalDeadline 覆盖：只有服务端权威 EXPIRED
// 才丢弃 pending，且当前 ACTIVE key 保持不变。
func TestAuthoritativeExpiredDiscardsPendingAfterLocalDeadline(t *testing.T) {
	store, manager, _ := pendingRecoveryFixture(t, false)
	active := store.Public().PublicKey
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatalf("recover expired: %v", err)
	}
	if store.Pending() != nil {
		t.Fatal("authoritatively expired pending not discarded")
	}
	if store.Public().PublicKey != active {
		t.Fatal("active key changed on expired rotation")
	}
	if _, err := store.CurrentPrivateKey(); err != nil {
		t.Fatalf("active key must remain usable: %v", err)
	}
}

// TestValidateRotationResponseRequiresActiveIdentity 覆盖：ACTIVE 必须携带完整且匹配的
// rotationId/keyId/newPublicKey，拒绝只有 ACTIVE+graceUntil 的空身份响应。
func TestValidateRotationResponseRequiresActiveIdentity(t *testing.T) {
	pending := &identity.PendingRotation{RotationID: "r", NewKeyID: "k", NewPublicKey: "p"}
	cases := []struct {
		name string
		resp QueryResponse
	}{
		{"empty-identity", QueryResponse{RotationID: "r", Status: "ACTIVE", GraceUntil: time.Now().Add(time.Minute).UnixMilli()}},
		{"missing-rotation-id", QueryResponse{Status: "ACTIVE", KeyID: "k", NewPublicKey: "p"}},
		{"missing-public-key", QueryResponse{RotationID: "r", Status: "ACTIVE", KeyID: "k"}},
		{"mismatched-key", QueryResponse{RotationID: "r", Status: "ACTIVE", KeyID: "other", NewPublicKey: "p"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRotationResponse(pending, &tc.resp); err == nil {
				t.Fatalf("incomplete ACTIVE identity accepted: %+v", tc.resp)
			}
		})
	}
}
