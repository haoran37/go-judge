package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
)

type Client struct {
	endpoint   string
	authToken  string
	httpClient *http.Client
	logger     logging.Logger
}

type CompileResult struct {
	ArtifactFileID string
	Status         int
	Message        string
}

type RunResult struct {
	Status     int
	TimeMS     int64
	MemoryKB   int64
	UserOutput string
	Message    string
}

type languageSpec struct {
	SourceName  string
	Artifact    string
	CompileArgs []string
	RunArgs     []string
	CompileEnv  []string
	RunEnv      []string
	Interpreted bool
}

type runRequest struct {
	RequestID string   `json:"requestId"`
	Cmd       []runCmd `json:"cmd"`
}

type runCmd struct {
	Args          []string           `json:"args"`
	Env           []string           `json:"env,omitempty"`
	Files         []*runCmdFile      `json:"files,omitempty"`
	CPULimit      uint64             `json:"cpuLimit"`
	ClockLimit    uint64             `json:"clockLimit"`
	MemoryLimit   uint64             `json:"memoryLimit"`
	StackLimit    uint64             `json:"stackLimit"`
	ProcLimit     uint64             `json:"procLimit"`
	CopyIn        map[string]runFile `json:"copyIn,omitempty"`
	CopyOut       []string           `json:"copyOut,omitempty"`
	CopyOutCached []string           `json:"copyOutCached,omitempty"`
	CopyOutMax    uint64             `json:"copyOutMax,omitempty"`
}

type runCmdFile struct {
	Content *string `json:"content,omitempty"`
	Name    *string `json:"name,omitempty"`
	Max     *int64  `json:"max,omitempty"`
}

type runFile struct {
	Content *string `json:"content,omitempty"`
	FileID  *string `json:"fileId,omitempty"`
}

type runResult struct {
	Status  string            `json:"status"`
	Time    uint64            `json:"time"`
	Memory  uint64            `json:"memory"`
	Files   map[string]string `json:"files,omitempty"`
	FileIDs map[string]string `json:"fileIds,omitempty"`
}

func New(endpoint, authToken string, httpClient *http.Client, logger logging.Logger) *Client {
	return &Client{
		endpoint:   strings.TrimRight(endpoint, "/"),
		authToken:  authToken,
		httpClient: httpClient,
		logger:     logger,
	}
}

func (c *Client) Compile(ctx context.Context, task model.Task) (CompileResult, error) {
	spec, err := specFor(task.Language)
	if err != nil {
		return CompileResult{Status: model.StatusSystemError, Message: err.Error()}, nil
	}
	if spec.Interpreted {
		return CompileResult{Status: model.StatusAccepted, ArtifactFileID: "", Message: ""}, nil
	}
	c.logger.Info("compile started", logging.String("submissionId", task.SubmissionID), logging.String("language", task.Language))

	stdout := "stdout"
	stderr := "stderr"
	max := int64(1 << 20)
	req := runRequest{
		RequestID: task.SubmissionID + ":compile",
		Cmd: []runCmd{{
			Args: spec.CompileArgs,
			Env:  spec.CompileEnv,
			Files: []*runCmdFile{
				{Content: strPtr("")},
				{Name: &stdout, Max: &max},
				{Name: &stderr, Max: &max},
			},
			CPULimit:    uint64(10 * time.Second),
			ClockLimit:  uint64(20 * time.Second),
			MemoryLimit: uint64(512 * 1024 * 1024),
			StackLimit:  uint64(256 * 1024 * 1024),
			ProcLimit:   128,
			CopyIn: map[string]runFile{
				spec.SourceName: {Content: &task.Code},
			},
			CopyOut:       []string{"stdout", "stderr"},
			CopyOutCached: []string{spec.Artifact},
			CopyOutMax:    1 << 24,
		}},
	}
	results, err := c.run(ctx, req)
	if err != nil {
		return CompileResult{}, err
	}
	if len(results) != 1 {
		return CompileResult{}, fmt.Errorf("unexpected compile result count %d", len(results))
	}
	res := results[0]
	if res.Status != "Accepted" {
		msg := strings.TrimSpace(res.Files["stderr"])
		if msg == "" {
			msg = res.Status
		}
		c.logger.Info("compile failed", logging.String("submissionId", task.SubmissionID), logging.String("status", res.Status))
		return CompileResult{Status: model.StatusCompileError, Message: msg}, nil
	}
	fileID := res.FileIDs[spec.Artifact]
	if fileID == "" {
		return CompileResult{Status: model.StatusSystemError, Message: "compile artifact missing"}, nil
	}
	return CompileResult{ArtifactFileID: fileID, Status: model.StatusAccepted}, nil
}

func (c *Client) RunCase(ctx context.Context, task model.Task, tc model.Case, artifactFileID string) (RunResult, error) {
	spec, err := specFor(task.Language)
	if err != nil {
		return RunResult{Status: model.StatusSystemError, Message: err.Error()}, nil
	}
	stdout := "stdout"
	stderr := "stderr"
	max := int64(16 << 20)
	copyIn := map[string]runFile{}
	if spec.Interpreted {
		copyIn[spec.SourceName] = runFile{Content: &task.Code}
	} else {
		copyIn[spec.Artifact] = runFile{FileID: &artifactFileID}
	}
	req := runRequest{
		RequestID: fmt.Sprintf("%s:case:%s", task.SubmissionID, tc.ID),
		Cmd: []runCmd{{
			Args: spec.RunArgs,
			Env:  spec.RunEnv,
			Files: []*runCmdFile{
				{Content: &tc.Input},
				{Name: &stdout, Max: &max},
				{Name: &stderr, Max: &max},
			},
			CPULimit:    uint64(time.Duration(task.TimeLimit) * time.Millisecond),
			ClockLimit:  uint64(time.Duration(task.TimeLimit*2+1000) * time.Millisecond),
			MemoryLimit: uint64(task.MemoryLimit * 1024 * 1024),
			StackLimit:  uint64(task.StackLimit * 1024 * 1024),
			ProcLimit:   128,
			CopyIn:      copyIn,
			CopyOut:     []string{"stdout", "stderr"},
			CopyOutMax:  16 << 20,
		}},
	}
	results, err := c.run(ctx, req)
	if err != nil {
		return RunResult{}, err
	}
	if len(results) != 1 {
		return RunResult{}, fmt.Errorf("unexpected run result count %d", len(results))
	}
	res := results[0]
	status := mapGoJudgeStatus(res.Status)
	output := res.Files["stdout"]
	if status == model.StatusAccepted && !sameOutput(output, tc.Expected, task.IsRemoveEndBlank) {
		status = model.StatusWrongAnswer
	}
	message := strings.TrimSpace(res.Files["stderr"])
	if message == "" && status != model.StatusAccepted {
		message = res.Status
	}
	return RunResult{
		Status:     status,
		TimeMS:     nsToMS(res.Time),
		MemoryKB:   bytesToKB(res.Memory),
		UserOutput: output,
		Message:    message,
	}, nil
}

func (c *Client) run(ctx context.Context, req runRequest) ([]runResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/run", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.authToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("go-judge /run failed with status %d", resp.StatusCode)
	}
	var results []runResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, err
	}
	return results, nil
}

func specFor(language string) (languageSpec, error) {
	switch strings.ToLower(language) {
	case "cpp", "cpp17", "c++", "c++17":
		return languageSpec{
			SourceName:  "main.cpp",
			Artifact:    "main",
			CompileArgs: []string{"/usr/bin/g++", "-O2", "-std=c++17", "main.cpp", "-o", "main", "-lm"},
			RunArgs:     []string{"./main"},
			CompileEnv:  defaultEnv(),
			RunEnv:      defaultEnv(),
		}, nil
	case "c", "c11":
		return languageSpec{
			SourceName:  "main.c",
			Artifact:    "main",
			CompileArgs: []string{"/usr/bin/gcc", "-O2", "-std=c11", "main.c", "-o", "main", "-lm"},
			RunArgs:     []string{"./main"},
			CompileEnv:  defaultEnv(),
			RunEnv:      defaultEnv(),
		}, nil
	case "java", "java17":
		return languageSpec{
			SourceName:  "Main.java",
			Artifact:    "Main.class",
			CompileArgs: []string{"/usr/bin/javac", "Main.java"},
			RunArgs:     []string{"/usr/bin/java", "Main"},
			CompileEnv:  defaultEnv(),
			RunEnv:      defaultEnv(),
		}, nil
	case "python", "python3", "py":
		return languageSpec{
			SourceName:  "main.py",
			RunArgs:     []string{"/usr/bin/python3", "main.py"},
			RunEnv:      defaultEnv(),
			Interpreted: true,
		}, nil
	default:
		return languageSpec{}, fmt.Errorf("unsupported language %q", language)
	}
}

func mapGoJudgeStatus(status string) int {
	switch status {
	case "Accepted":
		return model.StatusAccepted
	case "Wrong Answer", "Partially Correct":
		return model.StatusWrongAnswer
	case "Memory Limit Exceeded":
		return model.StatusMemoryLimitExceeded
	case "Time Limit Exceeded":
		return model.StatusTimeLimitExceeded
	case "Nonzero Exit Status", "Signalled", "Dangerous Syscall", "Output Limit Exceeded":
		return model.StatusRuntimeError
	default:
		return model.StatusSystemError
	}
}

func sameOutput(actual, expected string, removeEndBlank bool) bool {
	return normalizeOutput(actual, removeEndBlank) == normalizeOutput(expected, removeEndBlank)
}

func normalizeOutput(s string, removeEndBlank bool) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	if !removeEndBlank {
		return s
	}
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t\r")
	}
	return strings.Join(lines, "\n")
}

func defaultEnv() []string {
	return []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
}

func nsToMS(v uint64) int64 {
	if v == 0 {
		return 0
	}
	return int64((v + uint64(time.Millisecond) - 1) / uint64(time.Millisecond))
}

func bytesToKB(v uint64) int64 {
	if v == 0 {
		return 0
	}
	return int64((v + 1023) / 1024)
}

func strPtr(s string) *string {
	return &s
}
