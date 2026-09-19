package httpsign

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/identity"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
)

type staticCredential struct{}

func (staticCredential) Snapshot() auth.Credential {
	return auth.Credential{NodeID: "node-1", KeyID: "key-1", AccessToken: "access-token"}
}

func newStore(t *testing.T) *identity.Store {
	t.Helper()
	store, err := identity.Open(filepath.Join(t.TempDir(), "identity.json"), "n", "formal")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	return store
}

func TestSignerProducesVerifiableHTTPFields(t *testing.T) {
	store := newStore(t)
	signer := New(store, staticCredential{}, "judge.example.test", nil)
	signer.SetNow(func() time.Time { return time.UnixMilli(1789786000000) })

	var captured *http.Request
	var capturedBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	body := []byte(`{"a":1}`)
	resp, err := signer.Do(context.Background(), http.MethodPost, ts.URL+"/judge/problems/1/testdata?submissionId=s1&judgeTaskId=j1&attemptId=a1", body, nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	if captured.Header.Get("Authorization") != "Bearer access-token" {
		t.Fatalf("authorization %q", captured.Header.Get("Authorization"))
	}
	pathWithQuery := "/judge/problems/1/testdata?submissionId=s1&judgeTaskId=j1&attemptId=a1"
	fields := protocol.HTTPFields("judge.example.test", "node-1", "key-1", "POST", pathWithQuery,
		protocol.BodySHA256(capturedBody), captured.Header.Get("X-Judge-Timestamp"), captured.Header.Get("X-Judge-Nonce"))
	if err := protocol.Verify(store.PublicKey(), captured.Header.Get("X-Judge-Signature"), fields...); err != nil {
		t.Fatalf("signature verify: %v", err)
	}
	// 篡改 path 必须失败。
	tampered := protocol.HTTPFields("judge.example.test", "node-1", "key-1", "POST", pathWithQuery+"x",
		protocol.BodySHA256(capturedBody), captured.Header.Get("X-Judge-Timestamp"), captured.Header.Get("X-Judge-Nonce"))
	if err := protocol.Verify(store.PublicKey(), captured.Header.Get("X-Judge-Signature"), tampered...); err == nil {
		t.Fatal("tampered path verified")
	}
	// 篡改 body 必须失败。
	tamperedBody := protocol.HTTPFields("judge.example.test", "node-1", "key-1", "POST", pathWithQuery,
		protocol.BodySHA256([]byte(`{"a":2}`)), captured.Header.Get("X-Judge-Timestamp"), captured.Header.Get("X-Judge-Nonce"))
	if err := protocol.Verify(store.PublicKey(), captured.Header.Get("X-Judge-Signature"), tamperedBody...); err == nil {
		t.Fatal("tampered body verified")
	}
}

func TestClassifyStatus(t *testing.T) {
	if err := ClassifyStatus(http.StatusUnauthorized, []byte("nope")); !auth.IsDenied(err) {
		t.Fatalf("401 should be denied, got %v", err)
	}
	if err := ClassifyStatus(http.StatusForbidden, nil); !auth.IsDenied(err) {
		t.Fatalf("403 should be denied, got %v", err)
	}
	if err := ClassifyStatus(http.StatusInternalServerError, nil); err == nil || auth.IsDenied(err) {
		t.Fatalf("500 should be retryable, got %v", err)
	}
	if err := ClassifyStatus(http.StatusOK, nil); err != nil {
		t.Fatalf("200 should pass, got %v", err)
	}
}
