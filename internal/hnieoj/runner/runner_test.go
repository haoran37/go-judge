package runner

import (
	"testing"

	"github.com/criyle/go-judge/internal/hnieoj/model"
)

func TestNormalizeOutput(t *testing.T) {
	if !sameOutput("1 \r\n2\t\n\n", "1\n2\n", true) {
		t.Fatal("expected line-end blanks and final newline to be ignored")
	}
	if sameOutput("1  \n", "1\n", false) {
		t.Fatal("expected trailing spaces to matter when removeEndBlank is false")
	}
}

func TestMapGoJudgeStatus(t *testing.T) {
	cases := map[string]int{
		"Accepted":              model.StatusAccepted,
		"Memory Limit Exceeded": model.StatusMemoryLimitExceeded,
		"Time Limit Exceeded":   model.StatusTimeLimitExceeded,
		"Nonzero Exit Status":   model.StatusRuntimeError,
		"Internal Error":        model.StatusSystemError,
	}
	for status, want := range cases {
		if got := mapGoJudgeStatus(status); got != want {
			t.Fatalf("mapGoJudgeStatus(%q) = %d, want %d", status, got, want)
		}
	}
}

func TestSpecForLanguages(t *testing.T) {
	for _, language := range []string{"cpp", "c", "java17", "python3"} {
		if _, err := specFor(language); err != nil {
			t.Fatalf("specFor(%q) error: %v", language, err)
		}
	}
}

func TestJavaSpecPackagesClassFilesAsJar(t *testing.T) {
	spec, err := specFor("java17")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Artifact != "Main.jar" {
		t.Fatalf("java artifact = %q, want Main.jar", spec.Artifact)
	}
	if len(spec.RunArgs) < 4 || spec.RunArgs[2] != "Main.jar" {
		t.Fatalf("unexpected java run args: %#v", spec.RunArgs)
	}
}

func TestParseResultJSON(t *testing.T) {
	score := 7
	parsed, err := parseResultJSON(`{"status":"partially_correct","score":7,"message":"partial","diagnosticMessage":"detail"}`)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.status != model.StatusWrongAnswer || parsed.score == nil || *parsed.score != score || parsed.message != "partial" || parsed.diagnosticMessage != "detail" {
		t.Fatalf("unexpected parsed result: %+v", parsed)
	}
	if _, err := parseResultJSON(`{"status":"unknown"}`); err == nil {
		t.Fatal("expected unsupported status error")
	}
}

func TestRenderArguments(t *testing.T) {
	got, err := renderArguments(`"{input}" {expected} {actual} {result}`, "", map[string]string{
		"input":    "in file.txt",
		"expected": "expected.txt",
		"actual":   "actual.txt",
		"result":   "result.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"in file.txt", "expected.txt", "actual.txt", "result.json"}
	if len(got) != len(want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %#v, want %#v", got, want)
		}
	}
}

func TestSpecialResultFromInvalidJSONIsJudgementFailed(t *testing.T) {
	got, err := specialResultFromRun(runResult{
		Status: "Accepted",
		Files: map[string]string{
			resultFile: "{",
			stderrFile: "checker stderr",
		},
	}, "checker")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusJudgementFailed || got.DiagnosticMessage == "" {
		t.Fatalf("unexpected result: %+v", got)
	}
}
