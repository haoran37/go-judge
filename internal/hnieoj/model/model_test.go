package model

import (
	"encoding/json"
	"testing"
)

func TestTaskJSONCompatibilityWithBackendMessage(t *testing.T) {
	raw := `{
		"submissionId": "sub-1",
		"judgeTaskId": "task-1",
		"problemId": 7,
		"judgeMode": "default",
		"isRemoveEndBlank": true,
		"dataVersion": 3,
		"createdAt": "2026-09-18T10:00:00+08:00",
		"createdAtMillis": 1789000000000
	}`
	var task Task
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		t.Fatal(err)
	}
	if !task.IsRemoveEndBlank {
		t.Fatal("isRemoveEndBlank should map to IsRemoveEndBlank")
	}
	if task.CreatedAt.IsZero() {
		t.Fatal("createdAt with offset should parse")
	}
	if task.DataVersion != 3 || task.ProblemID != 7 {
		t.Fatalf("unexpected task: %+v", task)
	}
}

func TestTerminalEventEmitsExplicitZeroScoreAndAttempt(t *testing.T) {
	task := Task{SubmissionID: "sub-1", JudgeTaskID: "task-1", AttemptID: "attempt-1"}
	event := NewEvent(EventJudgeFinished, task, StatusWrongAnswer, 1, 1, 1, nil, "Judge finished")
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	score, ok := decoded["score"]
	if !ok {
		t.Fatalf("terminal event must include score explicitly: %s", raw)
	}
	if value, _ := score.(float64); value != 0 {
		t.Fatalf("score = %v, want 0", score)
	}
	if decoded["attemptId"] != "attempt-1" {
		t.Fatalf("attemptId = %v", decoded["attemptId"])
	}
}
