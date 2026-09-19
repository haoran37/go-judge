package testdata

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/httpsign"
	"github.com/criyle/go-judge/internal/hnieoj/identity"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
)

type cred struct{}

func (cred) Snapshot() auth.Credential {
	return auth.Credential{NodeID: "node-1", KeyID: "key-1", AccessToken: "token"}
}

func zipWithCase(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{"1.in": "input", "1.out": "output"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestEnsureSendsSignedTaskBoundRequest(t *testing.T) {
	store, err := identity.Open(filepath.Join(t.TempDir(), "identity.json"), "n", "formal")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	payload := zipWithCase(t)
	var verified bool
	var query string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		fields := protocol.HTTPFields("judge.example.test", "node-1", "key-1", http.MethodGet,
			r.URL.EscapedPath()+"?"+r.URL.RawQuery, protocol.BodySHA256(nil),
			r.Header.Get("X-Judge-Timestamp"), r.Header.Get("X-Judge-Nonce"))
		if err := protocol.Verify(store.PublicKey(), r.Header.Get("X-Judge-Signature"), fields...); err != nil {
			t.Errorf("signature verify: %v", err)
		} else {
			verified = true
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("X-Data-Version", "7")
		_, _ = w.Write(payload)
	}))
	defer ts.Close()

	signer := httpsign.New(store, cred{}, "judge.example.test", ts.Client())
	client := New(ts.URL, t.TempDir(), signer, logging.NopLogger{})
	cases, version, err := client.Ensure(context.Background(), model.Task{
		ProblemID: 1, SubmissionID: "s1", JudgeTaskID: "j1", AttemptID: "a1",
	})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !verified {
		t.Fatal("server did not verify signed request")
	}
	if len(cases) != 1 || cases[0].ID != "1" || version != 7 {
		t.Fatalf("cases %+v version %d", cases, version)
	}
	if query == "" ||
		!containsAll(query, "submissionId=s1", "judgeTaskId=j1", "attemptId=a1") {
		t.Fatalf("task binding query missing: %q", query)
	}
}

func TestEnsureAuthorizationFailureIsPermanent(t *testing.T) {
	store, _ := identity.Open(filepath.Join(t.TempDir(), "identity.json"), "n", "formal")
	_ = store.ApplyEnrollment("node-1", "key-1")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "denied")
	}))
	defer ts.Close()
	signer := httpsign.New(store, cred{}, "judge.example.test", ts.Client())
	client := New(ts.URL, t.TempDir(), signer, logging.NopLogger{})
	_, _, err := client.Ensure(context.Background(), model.Task{ProblemID: 1, SubmissionID: "s1", JudgeTaskID: "j1", AttemptID: "a1"})
	if err == nil {
		t.Fatal("expected permanent error")
	}
	var permanent ErrPermanent
	if !errors.As(err, &permanent) {
		t.Fatalf("expected ErrPermanent, got %T %v", err, err)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !bytes.Contains([]byte(value), []byte(part)) {
			return false
		}
	}
	return true
}
