package processor

import (
	"testing"

	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/runner"
)

func TestCaseScoreDistributesRemainder(t *testing.T) {
	task := model.Task{IOScore: 100}
	total := 3
	got := 0
	for i := 0; i < total; i++ {
		got += caseScore(task, total, i, model.StatusAccepted)
	}
	if got != 100 {
		t.Fatalf("total score = %d, want 100", got)
	}
	if first := caseScore(task, total, 0, model.StatusAccepted); first != 34 {
		t.Fatalf("first case score = %d, want 34", first)
	}
	if second := caseScore(task, total, 1, model.StatusAccepted); second != 33 {
		t.Fatalf("second case score = %d, want 33", second)
	}
}

func TestCaseScoreRejectedCaseGetsZero(t *testing.T) {
	task := model.Task{IOScore: 100}
	if got := caseScore(task, 3, 0, model.StatusWrongAnswer); got != 0 {
		t.Fatalf("wrong answer score = %d, want 0", got)
	}
}

func TestProcessorCaseScoreClipsMachineReadableScore(t *testing.T) {
	p := &Processor{}
	score := 99
	task := model.Task{IOScore: 10, ProblemType: model.ProblemTypeOI}
	got := p.caseScore(task, 2, 0, runner.RunResult{Status: model.StatusWrongAnswer, Score: &score})
	if got != 5 {
		t.Fatalf("score = %d, want clipped case max 5", got)
	}
	score = -1
	got = p.caseScore(task, 2, 0, runner.RunResult{Status: model.StatusWrongAnswer, Score: &score})
	if got != 0 {
		t.Fatalf("score = %d, want clipped min 0", got)
	}
}

func TestSupportedModeSetDefaultsToDefault(t *testing.T) {
	p := &Processor{supportedModes: supportedModeSet(nil)}
	if !p.supports(model.JudgeModeDefault) {
		t.Fatal("expected default mode support")
	}
	if p.supports(model.JudgeModeSPJ) {
		t.Fatal("did not expect spj mode support by default")
	}
}
