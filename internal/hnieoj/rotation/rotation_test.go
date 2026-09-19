package rotation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/httpsign"
	"github.com/criyle/go-judge/internal/hnieoj/identity"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
)

type rotationServer struct {
	mu                sync.Mutex
	rotationID        string
	newPublicKey      string
	keyID             string
	confirmNonce      string
	status            string
	graceUntil        int64
	prepareCalls      int
	failPrepare       bool
	failConfirm       bool
	activateOnConfirm bool
	verifyConfirm     bool
	verifyErr         error
}

func (s *rotationServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/judge/nodes/keys/rotations", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RotationID   string `json:"rotationId"`
			NewPublicKey string `json:"newPublicKey"`
			NewKeyProof  string `json:"newKeyProof"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.prepareCalls++
		if s.failPrepare {
			s.mu.Unlock()
			http.Error(w, "prepare unavailable", http.StatusBadGateway)
			return
		}
		if s.rotationID == "" {
			s.rotationID = body.RotationID
			s.newPublicKey = body.NewPublicKey
			s.keyID = "key-2"
			s.confirmNonce = "nonce-confirm"
			s.status = "PENDING"
		}
		resp := map[string]any{
			"rotationId": s.rotationID, "keyId": s.keyID,
			"confirmNonce": s.confirmNonce, "expiresAt": time.Now().Add(15 * time.Minute).UnixMilli(),
		}
		s.mu.Unlock()
		writeJSON(w, map[string]any{"code": 200, "msg": "ok", "data": resp})
	})
	mux.HandleFunc("/judge/nodes/keys/rotations/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/judge/nodes/keys/rotations/")
		if strings.HasSuffix(path, "/confirm") {
			s.handleConfirm(w, r)
			return
		}
		s.handleQuery(w)
	})
	return mux
}

func (s *rotationServer) handleConfirm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Signature string `json:"signature"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verifyConfirm {
		fields := protocol.RotateConfirmFields("judge.example.test", "node-1", s.rotationID, s.keyID, s.confirmNonce)
		if err := protocol.Verify(s.newPublicKey, body.Signature, fields...); err != nil {
			s.verifyErr = err
			http.Error(w, "bad confirm proof", http.StatusForbidden)
			return
		}
	}
	if s.failConfirm {
		// 模拟服务端已处理但响应丢失。
		s.status = "ACTIVE"
		if s.activateOnConfirm {
			s.graceUntil = time.Now().Add(5 * time.Minute).UnixMilli()
		}
		http.Error(w, "confirm response lost", http.StatusBadGateway)
		return
	}
	if s.status == "EXPIRED" {
		writeJSON(w, map[string]any{"code": 200, "msg": "ok", "data": map[string]any{"rotationId": s.rotationID, "keyId": s.keyID, "status": "EXPIRED"}})
		return
	}
	s.status = "ACTIVE"
	s.graceUntil = time.Now().Add(5 * time.Minute).UnixMilli()
	writeJSON(w, map[string]any{"code": 200, "msg": "ok", "data": map[string]any{
		"rotationId": s.rotationID, "keyId": s.keyID, "status": "ACTIVE",
		"newPublicKey": s.newPublicKey, "confirmNonce": s.confirmNonce, "graceUntil": s.graceUntil,
	}})
}

func (s *rotationServer) handleQuery(w http.ResponseWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, map[string]any{"code": 200, "msg": "ok", "data": map[string]any{
		"rotationId": s.rotationID, "keyId": s.keyID, "status": s.status,
		"newPublicKey": s.newPublicKey, "confirmNonce": s.confirmNonce, "graceUntil": s.graceUntil,
	}})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func setup(t *testing.T, srv *rotationServer) (*identity.Store, *Manager, *rotationServer) {
	t.Helper()
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	store, err := identity.Open(filepath.Join(t.TempDir(), "identity.json"), "n", "formal")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	cred := &auth.Credential{NodeID: "node-1", KeyID: "key-1"}
	signer := httpsign.New(store, cred, "judge.example.test", ts.Client())
	rotated := false
	manager := New(store, signer, cred, ts.URL, "judge.example.test", logging.NopLogger{}, Options{
		Enabled: true, Interval: time.Hour, Grace: 5 * time.Minute, ConfirmTimeout: 5 * time.Second,
	}, func() { rotated = true })
	_ = rotated
	return store, manager, srv
}

func TestRotateOnceSwitchesKeyAndKeepsOldInGrace(t *testing.T) {
	srv := &rotationServer{verifyConfirm: true}
	store, manager, _ := setup(t, srv)
	if err := manager.RotateOnce(context.Background()); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	pub := store.Public()
	if pub.KeyID != "key-2" {
		t.Fatalf("current key = %s", pub.KeyID)
	}
	if pub.GraceKeyID != "key-1" {
		t.Fatalf("old key not in grace: %+v", pub)
	}
	if srv.verifyErr != nil {
		t.Fatalf("confirm proof verify failed: %v", srv.verifyErr)
	}
	if store.Pending() != nil {
		t.Fatal("pending rotation should be cleared")
	}
}

func TestRecoverRetriesPrepareWithSameRotationID(t *testing.T) {
	srv := &rotationServer{verifyConfirm: true, failPrepare: true}
	store, manager, _ := setup(t, srv)
	// 模拟 prepare 响应丢失后崩溃：本地已有 pending，但不知道 keyId。
	if _, err := store.BeginRotation("rotation-fixed"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := manager.Recover(context.Background()); err == nil {
		t.Fatal("recover should fail while prepare is unavailable")
	}
	srv.mu.Lock()
	srv.failPrepare = false
	srv.mu.Unlock()
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if store.Public().KeyID != "key-2" {
		t.Fatalf("rotation not activated: %+v", store.Public())
	}
	if srv.rotationID != "rotation-fixed" {
		t.Fatalf("prepare must reuse persisted rotationId, got %s", srv.rotationID)
	}
	if srv.prepareCalls < 2 {
		t.Fatalf("expected prepare retry, calls=%d", srv.prepareCalls)
	}
}

func TestConfirmResponseLostRecoversViaQuery(t *testing.T) {
	srv := &rotationServer{verifyConfirm: true, failConfirm: true, activateOnConfirm: true}
	store, manager, _ := setup(t, srv)
	if err := manager.RotateOnce(context.Background()); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if store.Public().KeyID != "key-2" {
		t.Fatalf("rotation not activated after lost confirm: %+v", store.Public())
	}
}

func TestExpiredPendingKeepsActiveKey(t *testing.T) {
	srv := &rotationServer{status: "EXPIRED", rotationID: "rotation-expired", keyID: "key-2"}
	store, manager, _ := setup(t, srv)
	active := store.Public().PublicKey
	if _, err := store.BeginRotation("rotation-expired"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := store.RecordRotationPrepared("rotation-expired", "key-2", "nonce", time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if store.Pending() != nil {
		t.Fatal("expired pending must be cleared")
	}
	if store.Public().PublicKey != active {
		t.Fatal("active key must not change on expired rotation")
	}
	if _, err := store.CurrentPrivateKey(); err != nil {
		t.Fatalf("active key must remain usable: %v", err)
	}
}
