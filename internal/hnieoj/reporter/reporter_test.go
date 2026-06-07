package reporter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
)

type staticCredential struct{}

func (staticCredential) Apply(req *http.Request) {
	req.Header.Set("X-Judge-Token", "token")
}

func TestHTTPReporterSendsEventWithAuthAndIdempotencyKey(t *testing.T) {
	type requestInfo struct {
		auth  string
		key   string
		path  string
		event model.Event
		err   error
	}
	requests := make(chan requestInfo, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event model.Event
		err := json.NewDecoder(r.Body).Decode(&event)
		requests <- requestInfo{
			auth:  r.Header.Get("X-Judge-Token"),
			key:   r.Header.Get("Idempotency-Key"),
			path:  r.URL.Path,
			event: event,
			err:   err,
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	rep := NewHTTP(server.URL, "/judge/submissions/{submissionId}/events", server.Client(), staticCredential{}, logging.NopLogger{})
	task := model.Task{SubmissionID: "sub-1", JudgeTaskID: "task-1"}
	event := model.NewEvent(model.EventJudgeFinished, task, model.StatusAccepted, 1, 1, 1, nil, "done")
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
	if got.event.DiagnosticMessage != "hidden detail" {
		t.Fatalf("diagnosticMessage = %q", got.event.DiagnosticMessage)
	}
}
