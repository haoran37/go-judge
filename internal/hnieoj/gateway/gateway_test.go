package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
)

type fakeCredential struct {
	header  string
	expired bool
}

func (f fakeCredential) Apply(req *http.Request) {
	if f.header != "" {
		req.Header.Set("Authorization", f.header)
	}
}

func (f fakeCredential) Expired(time.Time) bool { return f.expired }

func writeEnvelope(t *testing.T, w http.ResponseWriter, code int, msg string, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": msg, "data": data})
}

func TestClaimParsesEnvelopeAndSendsBearer(t *testing.T) {
	var gotAuth string
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/judge/tasks/claim" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, 16)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		writeEnvelope(t, w, 200, "success", map[string]any{
			"task": map[string]any{
				"submissionId": "sub-1", "judgeTaskId": "task-1", "problemId": 7,
				"judgeMode": "default", "isRemoveEndBlank": true, "createdAt": "2026-09-18T10:00:00+08:00",
			},
			"attemptId": "attempt-1", "leaseUntil": 1234567890, "renewAfterMillis": 20000,
		})
	}))
	defer server.Close()

	client := New(server.URL, server.Client(), fakeCredential{header: "Bearer node-jwt"}, logging.NopLogger{})
	claim, err := client.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer node-jwt" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotBody != "{}" {
		t.Fatalf("body = %q, want {}", gotBody)
	}
	if claim == nil || claim.AttemptID != "attempt-1" || claim.LeaseUntil != 1234567890 || claim.RenewAfterMillis != 20000 {
		t.Fatalf("unexpected claim: %+v", claim)
	}
	if claim.Task.AttemptID != "attempt-1" || claim.Task.SubmissionID != "sub-1" || !claim.Task.IsRemoveEndBlank {
		t.Fatalf("unexpected task: %+v", claim.Task)
	}
	if claim.Task.CreatedAt.IsZero() {
		t.Fatal("createdAt with offset should parse")
	}
}

func TestClaimNullDataMeansEmptyQueue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"msg":"success","data":null}`))
	}))
	defer server.Close()
	client := New(server.URL, server.Client(), fakeCredential{}, logging.NopLogger{})
	claim, err := client.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if claim != nil {
		t.Fatalf("expected nil claim for empty queue, got %+v", claim)
	}
}

func TestClaimFailsOnBusinessErrorAndDenied(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     int
		wantDeny bool
	}{
		{"business", 500, false},
		{"forbidden", 403, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeEnvelope(t, w, tc.code, "nope", nil)
			}))
			defer server.Close()
			client := New(server.URL, server.Client(), fakeCredential{}, logging.NopLogger{})
			_, err := client.Claim(context.Background())
			if err == nil {
				t.Fatal("expected error")
			}
			if auth.IsDenied(err) != tc.wantDeny {
				t.Fatalf("IsDenied = %v, want %v (err %v)", auth.IsDenied(err), tc.wantDeny, err)
			}
		})
	}
}

func TestClaimFailsOnMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer server.Close()
	client := New(server.URL, server.Client(), fakeCredential{}, logging.NopLogger{})
	if _, err := client.Claim(context.Background()); err == nil {
		t.Fatal("expected malformed claim response to fail")
	}
}

func TestClaimFailsWhenCredentialExpired(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	client := New(server.URL, server.Client(), fakeCredential{expired: true}, logging.NopLogger{})
	if _, err := client.Claim(context.Background()); err == nil {
		t.Fatal("expected expired credential error")
	}
	if called {
		t.Fatal("expired credential must not issue a claim request")
	}
}

func TestRenewSendsTaskFieldsAndParsesLease(t *testing.T) {
	var body map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/judge/tasks/sub-1/lease" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeEnvelope(t, w, 200, "success", map[string]any{"leaseUntil": 987654321})
	}))
	defer server.Close()

	client := New(server.URL, server.Client(), fakeCredential{header: "Bearer jwt"}, logging.NopLogger{})
	until, err := client.Renew(context.Background(), "sub-1", "task-1", "attempt-1")
	if err != nil {
		t.Fatal(err)
	}
	if until != 987654321 {
		t.Fatalf("leaseUntil = %d", until)
	}
	if body["judgeTaskId"] != "task-1" || body["attemptId"] != "attempt-1" {
		t.Fatalf("unexpected renew body: %#v", body)
	}
}

func TestRenewRejectsEmptyLeaseAndDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/denied" {
			writeEnvelope(t, w, 403, "revoked", nil)
			return
		}
		writeEnvelope(t, w, 200, "success", map[string]any{})
	}))
	defer server.Close()
	client := New(server.URL, server.Client(), fakeCredential{}, logging.NopLogger{})
	if _, err := client.Renew(context.Background(), "sub-1", "task-1", "attempt-1"); err == nil {
		t.Fatal("expected empty lease payload to fail")
	}
	deniedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, 403, "revoked", nil)
	}))
	defer deniedServer.Close()
	deniedClient := New(deniedServer.URL, deniedServer.Client(), fakeCredential{}, logging.NopLogger{})
	if _, err := deniedClient.Renew(context.Background(), "s", "t", "a"); !auth.IsDenied(err) {
		t.Fatalf("expected denied, got %v", err)
	}
}
