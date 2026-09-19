package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTaskJSONCompatibilityWithBackendMessage(t *testing.T) {
	task := Task{
		SubmissionID: "s1", JudgeTaskID: "j1", AttemptID: "a1",
		JudgeID: 10, ProblemID: 20, JudgeMode: "spj", IOScore: 100,
		ProblemType: ProblemTypeACM, CreatedAt: time.Unix(0, 0).UTC(),
	}
	raw, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{`"submissionId":"s1"`, `"judgeTaskId":"j1"`, `"attemptId":"a1"`, `"judgeMode":"spj"`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("task json missing %s: %s", field, raw)
		}
	}
	var decoded Task
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.SubmissionID != task.SubmissionID || decoded.AttemptID != task.AttemptID {
		t.Fatalf("round trip mismatch: %+v", decoded)
	}
}

func TestTerminalEventEmitsExplicitZeroScoreAndAttempt(t *testing.T) {
	task := Task{SubmissionID: "s1", JudgeTaskID: "j1", AttemptID: "a1"}
	event := NewEvent(EventJudgeFinished, task, StatusWrongAnswer, 3, 3, 3, nil, "done")
	event.Score = 0
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"score":0`) {
		t.Fatalf("terminal event must include explicit zero score: %s", raw)
	}
	if !strings.Contains(string(raw), `"attemptId":"a1"`) {
		t.Fatalf("terminal event must include attempt: %s", raw)
	}
}
