package testdata

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
)

type stubCredential struct{}

func (stubCredential) Apply(req *http.Request) { req.Header.Set("Authorization", "Bearer node") }

func zipWithCase(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{"1.in": "a", "1.out": "a"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEnsureSendsTaskQualificationQuery(t *testing.T) {
	payload := zipWithCase(t)
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("X-Data-Version", "3")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	client := New(server.URL, t.TempDir(), server.Client(), stubCredential{}, logging.NopLogger{})
	cases, version, err := client.Ensure(context.Background(), model.Task{
		SubmissionID: "sub-1", JudgeTaskID: "task-1", AttemptID: "attempt-1", ProblemID: 7, DataVersion: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || version != 3 {
		t.Fatalf("cases = %+v version = %d", cases, version)
	}
	if gotQuery.Get("submissionId") != "sub-1" || gotQuery.Get("judgeTaskId") != "task-1" || gotQuery.Get("attemptId") != "attempt-1" {
		t.Fatalf("missing task qualification query: %v", gotQuery)
	}
	if gotQuery.Get("version") != "" {
		t.Fatalf("first download should not pin a local version, got %q", gotQuery.Get("version"))
	}
}

func TestEnsureAuthorizationFailureIsPermanent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":403,"msg":"lease lost"}`))
	}))
	defer server.Close()

	client := New(server.URL, t.TempDir(), server.Client(), stubCredential{}, logging.NopLogger{})
	_, _, err := client.Ensure(context.Background(), model.Task{SubmissionID: "sub-1", JudgeTaskID: "task-1", AttemptID: "attempt-1", ProblemID: 7})
	var permanent ErrPermanent
	if !errors.As(err, &permanent) {
		t.Fatalf("expected ErrPermanent for 403, got %v", err)
	}
}
