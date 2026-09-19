package enrollment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/identity"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
)

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func TestEnrollSignsChallengeAndPersistsIdentity(t *testing.T) {
	dir := t.TempDir()
	bootstrapPath := filepath.Join(dir, "bootstrap.token")
	if err := os.WriteFile(bootstrapPath, []byte("bootstrap-secret"), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}
	store, err := identity.Open(filepath.Join(dir, "identity.json"), "node-a", "formal")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	var failFirst atomic.Bool
	failFirst.Store(true)
	var enrollCalls atomic.Int32
	var verified atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/judge/nodes/enrollment-challenges":
			writeJSON(w, map[string]any{"code": 200, "msg": "ok", "data": map[string]any{
				"challengeId": "challenge-1",
				"nonce":       "bm9uY2U=",
				"audience":    "judge.example.test",
				"expiresAt":   time.Now().Add(time.Minute).UnixMilli(),
				"serverTime":  time.Now().UnixMilli(),
			}})
		case "/judge/nodes/enroll":
			enrollCalls.Add(1)
			if failFirst.Load() {
				failFirst.Store(false)
				http.Error(w, "gateway lost response", http.StatusBadGateway)
				return
			}
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			fields := protocol.EnrollFields("judge.example.test", "challenge-1", "bm9uY2U=",
				store.EnrollmentID(), "node-a", store.PublicKey(), protocol.BootstrapDigest("bootstrap-secret"))
			if err := protocol.Verify(store.PublicKey(), req["signature"], fields...); err != nil {
				t.Errorf("enroll signature invalid: %v", err)
			} else {
				verified.Store(true)
			}
			writeJSON(w, map[string]any{"code": 200, "msg": "ok", "data": map[string]any{
				"nodeId": "node-1", "keyId": "key-1", "nodeType": "formal",
				"maxConcurrency": 2, "supportedJudgeModes": []string{"default"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := New(ts.URL, "node-a", store, Bootstrap{TokenFile: bootstrapPath}, ts.Client(), logging.NopLogger{})
	client.SetNow(time.Now)
	if err := client.EnsureEnrolled(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("ensure enrolled: %v", err)
	}
	if enrollCalls.Load() < 2 {
		t.Fatalf("expected a retry after lost response, calls=%d", enrollCalls.Load())
	}
	if !verified.Load() {
		t.Fatal("server did not verify enrollment signature")
	}
	if store.NodeID() != "node-1" || store.CurrentKeyID() != "key-1" {
		t.Fatalf("identity not persisted: %s/%s", store.NodeID(), store.CurrentKeyID())
	}
	if _, err := os.Stat(bootstrapPath); !os.IsNotExist(err) {
		t.Fatalf("bootstrap should be consumed after success, stat err=%v", err)
	}
	// 已登记后不再调用注册端点。
	before := enrollCalls.Load()
	if err := client.EnsureEnrolled(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if enrollCalls.Load() != before {
		t.Fatal("already-enrolled node must not re-enroll")
	}
}

func TestEnrollDeniedIsNotRetried(t *testing.T) {
	dir := t.TempDir()
	store, _ := identity.Open(filepath.Join(dir, "identity.json"), "n", "formal")
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, map[string]any{"code": 403, "msg": "revoked"})
	}))
	defer ts.Close()
	client := New(ts.URL, "n", store, Bootstrap{Token: "bootstrap"}, ts.Client(), logging.NopLogger{})
	_, err := client.Enroll(context.Background())
	if err == nil {
		t.Fatal("expected denial")
	}
	if calls.Load() > 2 {
		t.Fatalf("unexpected extra calls: %d", calls.Load())
	}
	if store.NodeID() != "" {
		t.Fatal("denied enrollment must not assign identity")
	}
}
