package reporter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
)

type staticCredential struct{}

func (staticCredential) Apply(req *http.Request) {
	req.Header.Set("X-Judge-Token", "token")
}

func successEnvelope(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "msg": "success", "data": nil})
}

func TestHTTPReporterSendsEventWithAuthIdempotencyAndExplicitZeroScore(t *testing.T) {
	type requestInfo struct {
		auth string
		key  string
		path string
		raw  map[string]any
		err  error
	}
	requests := make(chan requestInfo, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]any
		err := json.NewDecoder(r.Body).Decode(&raw)
		requests <- requestInfo{
			auth: r.Header.Get("X-Judge-Token"),
			key:  r.Header.Get("Idempotency-Key"),
			path: r.URL.Path,
			raw:  raw,
			err:  err,
		}
		successEnvelope(w)
	}))
	defer server.Close()

	rep := NewHTTP(server.URL, "/judge/submissions/{submissionId}/events", server.Client(), staticCredential{}, logging.NopLogger{}, 3, time.Millisecond)
	task := model.Task{SubmissionID: "sub-1", JudgeTaskID: "task-1", AttemptID: "attempt-1"}
	event := model.NewEvent(model.EventJudgeFinished, task, model.StatusWrongAnswer, 1, 1, 1, nil, "done")
	event.DiagnosticMessage = "hidden detail"
	if err := rep.ReportJudgeFinished(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	got := <-requests
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.auth != "token" {
		t.Fatalf("auth header = %q, want token", got.auth)
	}
	if got.key != "sub-1:task-1:JUDGE_FINISHED:1:1" {
		t.Fatalf("idempotency key = %q", got.key)
	}
	if got.path != "/judge/submissions/sub-1/events" {
		t.Fatalf("path = %q", got.path)
	}
	if got.raw["diagnosticMessage"] != "hidden detail" || got.raw["attemptId"] != "attempt-1" {
		t.Fatalf("unexpected payload: %#v", got.raw)
	}
	// WA 的 0 分必须显式发送，不能被 omitempty 省略。
	score, ok := got.raw["score"]
	if !ok {
		t.Fatalf("score missing from terminal payload: %#v", got.raw)
	}
	if scoreValue, _ := score.(float64); scoreValue != 0 {
		t.Fatalf("score = %#v, want 0", score)
	}
}

func TestHTTPReporterFailsOnHTTP200BusinessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 403, "msg": "forbidden", "data": nil})
	}))
	defer server.Close()

	rep := NewHTTP(server.URL, "/judge/submissions/{submissionId}/events", server.Client(), staticCredential{}, logging.NopLogger{}, 3, time.Millisecond)
	event := model.NewEvent(model.EventJudgeFinished, model.Task{SubmissionID: "sub-1", JudgeTaskID: "task-1", AttemptID: "a"}, model.StatusAccepted, 1, 1, 1, nil, "done")
	if err := rep.ReportJudgeFinished(context.Background(), event); err == nil {
		t.Fatal("expected HTTP200 code!=200 to fail")
	}
}

func TestHTTPReporterFailsOnMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json"))
	}))
	defer server.Close()

	rep := NewHTTP(server.URL, "/judge/submissions/{submissionId}/events", server.Client(), staticCredential{}, logging.NopLogger{}, 0, time.Millisecond)
	event := model.NewEvent(model.EventStatusChanged, model.Task{SubmissionID: "sub-1", JudgeTaskID: "task-1"}, model.StatusRunning, 1, 0, 0, nil, "running")
	if err := rep.ReportStatusChanged(context.Background(), event); err == nil {
		t.Fatal("expected malformed response to fail")
	}
}

func TestHTTPReporterRetriesTransient5xx(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		successEnvelope(w)
	}))
	defer server.Close()

	rep := NewHTTP(server.URL, "/judge/submissions/{submissionId}/events", server.Client(), staticCredential{}, logging.NopLogger{}, 2, time.Millisecond)
	event := model.NewEvent(model.EventJudgeFinished, model.Task{SubmissionID: "sub-1", JudgeTaskID: "task-1"}, model.StatusAccepted, 1, 1, 1, nil, "done")
	if err := rep.ReportJudgeFinished(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}
