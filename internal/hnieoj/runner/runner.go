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
	"github.com/google/shlex"
)

const (
	stdoutFile = "stdout"
	stderrFile = "stderr"
	resultFile = "result.json"
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
	Status            int
	TimeMS            int64
	MemoryKB          int64
	UserOutput        string
	Message           string
	DiagnosticMessage string
	Score             *int
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
	RequestID   string    `json:"requestId"`
	Cmd         []runCmd  `json:"cmd"`
	PipeMapping []pipeMap `json:"pipeMapping,omitempty"`
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

type pipeIndex struct {
	Index int `json:"index"`
	Fd    int `json:"fd"`
}

type pipeMap struct {
	In              pipeIndex `json:"in"`
	Out             pipeIndex `json:"out"`
	Name            string    `json:"name,omitempty"`
	Max             int64     `json:"max,omitempty"`
	Proxy           bool      `json:"proxy,omitempty"`
	DisableZeroCopy bool      `json:"disableZeroCopy,omitempty"`
}

type runResult struct {
	Status     string            `json:"status"`
	ExitStatus int               `json:"exitStatus"`
	Time       uint64            `json:"time"`
	Memory     uint64            `json:"memory"`
	Files      map[string]string `json:"files,omitempty"`
	FileIDs    map[string]string `json:"fileIds,omitempty"`
}

type programLimits struct {
	TimeMS      int64
	MemoryMB    int64
	StackMB     int64
	OutputBytes int64
	ProcLimit   uint64
}

type resultJSON struct {
	Status            string `json:"status"`
	Score             *int   `json:"score,omitempty"`
	Message           string `json:"message,omitempty"`
	DiagnosticMessage string `json:"diagnosticMessage,omitempty"`
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
	return c.CompileProgram(ctx, task.SubmissionID+":compile", task.Language, task.Code)
}

func (c *Client) CompileProgram(ctx context.Context, requestID, language, source string) (CompileResult, error) {
	spec, err := specFor(language)
	if err != nil {
		return CompileResult{Status: model.StatusSystemError, Message: err.Error()}, nil
	}
	if spec.Interpreted {
		return CompileResult{Status: model.StatusAccepted}, nil
	}
	if strings.TrimSpace(source) == "" {
		return CompileResult{Status: model.StatusSystemError, Message: "program source is required"}, nil
	}
	c.logger.Info("compile started", logging.String("requestId", requestID), logging.String("language", language))

	max := int64(1 << 20)
	req := runRequest{
		RequestID: requestID,
		Cmd: []runCmd{{
			Args: spec.CompileArgs,
			Env:  spec.CompileEnv,
			Files: []*runCmdFile{
				{Content: strPtr("")},
				{Name: strPtr(stdoutFile), Max: &max},
				{Name: strPtr(stderrFile), Max: &max},
			},
			CPULimit:    uint64(10 * time.Second),
			ClockLimit:  uint64(20 * time.Second),
			MemoryLimit: uint64(512 * 1024 * 1024),
			StackLimit:  uint64(256 * 1024 * 1024),
			ProcLimit:   128,
			CopyIn: map[string]runFile{
				spec.SourceName: {Content: &source},
			},
			CopyOut:       []string{stdoutFile, stderrFile},
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
		msg := strings.TrimSpace(res.Files[stderrFile])
		if msg == "" {
			msg = res.Status
		}
		c.logger.Info("compile failed", logging.String("requestId", requestID), logging.String("status", res.Status))
		return CompileResult{Status: model.StatusCompileError, Message: msg}, nil
	}
	fileID := res.FileIDs[spec.Artifact]
	if fileID == "" {
		return CompileResult{Status: model.StatusSystemError, Message: "compile artifact missing"}, nil
	}
	return CompileResult{ArtifactFileID: fileID, Status: model.StatusAccepted}, nil
}

func (c *Client) DeleteFile(ctx context.Context, fileID string) error {
	if fileID == "" {
		return nil
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint+"/file/"+fileID, nil)
	if err != nil {
		return err
	}
	if c.authToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("go-judge DELETE /file/%s failed with status %d", fileID, resp.StatusCode)
	}
	return nil
}

func (c *Client) RunCase(ctx context.Context, task model.Task, tc model.Case, artifactFileID string) (RunResult, error) {
	runResult, err := c.runUserCase(ctx, task, tc, artifactFileID)
	if err != nil {
		return RunResult{}, err
	}
	if runResult.Status == model.StatusAccepted && !sameOutput(runResult.UserOutput, tc.Expected, task.IsRemoveEndBlank) {
		runResult.Status = model.StatusWrongAnswer
	}
	return runResult, nil
}

func (c *Client) RunSPJCase(ctx context.Context, task model.Task, tc model.Case, userArtifactID, checkerArtifactID string) (RunResult, error) {
	if task.Checker == nil {
		return RunResult{Status: model.StatusJudgementFailed, Message: "checker is required"}, nil
	}
	userResult, err := c.runUserCase(ctx, task, tc, userArtifactID)
	if err != nil {
		return RunResult{}, err
	}
	if userResult.Status != model.StatusAccepted {
		return userResult, nil
	}

	checkerCmd, err := c.programCmd(*task.Checker, checkerArtifactID, programLimitsFromProgram(*task.Checker), map[string]runFile{
		"input.txt":    {Content: &tc.Input},
		"expected.txt": {Content: &tc.Expected},
		"actual.txt":   {Content: &userResult.UserOutput},
	}, []string{stdoutFile, stderrFile, resultFile + "?"}, resultFile)
	if err != nil {
		return RunResult{Status: model.StatusJudgementFailed, Message: err.Error()}, nil
	}
	args, err := renderArguments(task.Checker.ArgumentTemplate, "{input} {expected} {actual} {result}", map[string]string{
		"input":    "input.txt",
		"expected": "expected.txt",
		"actual":   "actual.txt",
		"result":   resultFile,
	})
	if err != nil {
		return RunResult{Status: model.StatusJudgementFailed, Message: "invalid checker argumentTemplate", DiagnosticMessage: err.Error()}, nil
	}
	checkerCmd.Args = append(checkerCmd.Args, args...)

	results, err := c.run(ctx, runRequest{
		RequestID: fmt.Sprintf("%s:case:%s:checker", task.SubmissionID, tc.ID),
		Cmd:       []runCmd{checkerCmd},
	})
	if err != nil {
		return RunResult{}, err
	}
	if len(results) != 1 {
		return RunResult{}, fmt.Errorf("unexpected checker result count %d", len(results))
	}
	return specialResultFromRun(results[0], "checker")
}

func (c *Client) RunInteractiveCase(ctx context.Context, task model.Task, tc model.Case, userArtifactID, interactorArtifactID string) (RunResult, error) {
	if task.Interactor == nil {
		return RunResult{Status: model.StatusInvalidInteraction, Message: "interactor is required"}, nil
	}

	userCmd, err := c.userCmd(task, userArtifactID, programLimitsFromTask(task), []*runCmdFile{
		nil,
		nil,
		{Name: strPtr("user_stderr"), Max: int64Ptr(16 << 20)},
	}, []string{"user_stderr"})
	if err != nil {
		return RunResult{Status: model.StatusSystemError, Message: err.Error()}, nil
	}
	interactorCmd, err := c.programCmd(*task.Interactor, interactorArtifactID, programLimitsFromProgram(*task.Interactor), map[string]runFile{
		"input.txt":    {Content: &tc.Input},
		"expected.txt": {Content: &tc.Expected},
	}, []string{"interactor_stderr", resultFile + "?"}, resultFile,
		nil,
		nil,
		&runCmdFile{Name: strPtr("interactor_stderr"), Max: int64Ptr(16 << 20)},
	)
	if err != nil {
		return RunResult{Status: model.StatusInvalidInteraction, Message: err.Error()}, nil
	}
	args, err := renderArguments(task.Interactor.ArgumentTemplate, "{input} {expected} {result}", map[string]string{
		"input":    "input.txt",
		"expected": "expected.txt",
		"result":   resultFile,
	})
	if err != nil {
		return RunResult{Status: model.StatusInvalidInteraction, Message: "invalid interactor argumentTemplate", DiagnosticMessage: err.Error()}, nil
	}
	interactorCmd.Args = append(interactorCmd.Args, args...)

	results, err := c.run(ctx, runRequest{
		RequestID: fmt.Sprintf("%s:case:%s:interactive", task.SubmissionID, tc.ID),
		Cmd:       []runCmd{userCmd, interactorCmd},
		PipeMapping: []pipeMap{
			{In: pipeIndex{Index: 0, Fd: 1}, Out: pipeIndex{Index: 1, Fd: 0}},
			{In: pipeIndex{Index: 1, Fd: 1}, Out: pipeIndex{Index: 0, Fd: 0}},
		},
	})
	if err != nil {
		return RunResult{}, err
	}
	if len(results) != 2 {
		return RunResult{}, fmt.Errorf("unexpected interactive result count %d", len(results))
	}
	userStatus := mapGoJudgeStatus(results[0].Status)
	interactorResult, err := specialResultFromRun(results[1], "interactor")
	if err != nil {
		return RunResult{}, err
	}
	interactorResult.TimeMS = nsToMS(maxUint64(results[0].Time, results[1].Time))
	interactorResult.MemoryKB = bytesToKB(maxUint64(results[0].Memory, results[1].Memory))
	interactorResult.UserOutput = ""
	if interactorResult.Status != model.StatusAccepted {
		return interactorResult, nil
	}
	if userStatus != model.StatusAccepted {
		interactorResult.Status = userStatus
		interactorResult.Message = strings.TrimSpace(results[0].Files["user_stderr"])
		if interactorResult.Message == "" {
			interactorResult.Message = results[0].Status
		}
	}
	return interactorResult, nil
}

func (c *Client) runUserCase(ctx context.Context, task model.Task, tc model.Case, artifactFileID string) (RunResult, error) {
	cmd, err := c.userCmd(task, artifactFileID, programLimitsFromTask(task), []*runCmdFile{
		{Content: &tc.Input},
		{Name: strPtr(stdoutFile), Max: int64Ptr(16 << 20)},
		{Name: strPtr(stderrFile), Max: int64Ptr(16 << 20)},
	}, []string{stdoutFile, stderrFile})
	if err != nil {
		return RunResult{Status: model.StatusSystemError, Message: err.Error()}, nil
	}
	results, err := c.run(ctx, runRequest{
		RequestID: fmt.Sprintf("%s:case:%s", task.SubmissionID, tc.ID),
		Cmd:       []runCmd{cmd},
	})
	if err != nil {
		return RunResult{}, err
	}
	if len(results) != 1 {
		return RunResult{}, fmt.Errorf("unexpected run result count %d", len(results))
	}
	res := results[0]
	status := mapGoJudgeStatus(res.Status)
	message := strings.TrimSpace(res.Files[stderrFile])
	if message == "" && status != model.StatusAccepted {
		message = res.Status
	}
	return RunResult{
		Status:     status,
		TimeMS:     nsToMS(res.Time),
		MemoryKB:   bytesToKB(res.Memory),
		UserOutput: res.Files[stdoutFile],
		Message:    message,
	}, nil
}

func (c *Client) userCmd(task model.Task, artifactFileID string, limits programLimits, files []*runCmdFile, copyOut []string) (runCmd, error) {
	prog := model.Program{
		Language:    task.Language,
		Source:      task.Code,
		TimeLimit:   task.TimeLimit,
		MemoryLimit: task.MemoryLimit,
		StackLimit:  task.StackLimit,
	}
	return c.programCmd(prog, artifactFileID, limits, nil, copyOut, "", files...)
}

func (c *Client) programCmd(program model.Program, artifactFileID string, limits programLimits, copyIn map[string]runFile, copyOut []string, resultFileName string, filesOverride ...*runCmdFile) (runCmd, error) {
	spec, err := specFor(program.Language)
	if err != nil {
		return runCmd{}, err
	}
	if copyIn == nil {
		copyIn = map[string]runFile{}
	}
	if spec.Interpreted {
		copyIn[spec.SourceName] = runFile{Content: &program.Source}
	} else {
		copyIn[spec.Artifact] = runFile{FileID: &artifactFileID}
	}
	files := filesOverride
	if len(files) == 0 {
		files = []*runCmdFile{
			{Content: strPtr("")},
			{Name: strPtr(stdoutFile), Max: int64Ptr(limitOrDefault(limits.OutputBytes, 16<<20))},
			{Name: strPtr(stderrFile), Max: int64Ptr(16 << 20)},
		}
	}
	return runCmd{
		Args:        append([]string{}, spec.RunArgs...),
		Env:         spec.RunEnv,
		Files:       files,
		CPULimit:    uint64(time.Duration(limitOrDefault(limits.TimeMS, 2000)) * time.Millisecond),
		ClockLimit:  uint64(time.Duration(limitOrDefault(limits.TimeMS, 2000)*2+1000) * time.Millisecond),
		MemoryLimit: uint64(limitOrDefault(limits.MemoryMB, 512) * 1024 * 1024),
		StackLimit:  uint64(limitOrDefault(limits.StackMB, 256) * 1024 * 1024),
		ProcLimit:   limitProc(limits.ProcLimit),
		CopyIn:      copyIn,
		CopyOut:     copyOut,
		CopyOutMax:  uint64(limitOrDefault(limits.OutputBytes, 16<<20)),
	}, nil
}

func specialResultFromRun(res runResult, role string) (RunResult, error) {
	stderr := strings.TrimSpace(res.Files[stderrFile])
	if role == "interactor" {
		stderr = strings.TrimSpace(res.Files["interactor_stderr"])
	}
	if raw, ok := res.Files[resultFile]; ok && strings.TrimSpace(raw) != "" {
		parsed, err := parseResultJSON(raw)
		if err != nil {
			return RunResult{
				Status:            model.StatusJudgementFailed,
				TimeMS:            nsToMS(res.Time),
				MemoryKB:          bytesToKB(res.Memory),
				Message:           role + " returned invalid result.json",
				DiagnosticMessage: joinDiagnostic(err.Error(), stderr),
			}, nil
		}
		return RunResult{
			Status:            parsed.status,
			TimeMS:            nsToMS(res.Time),
			MemoryKB:          bytesToKB(res.Memory),
			Message:           parsed.message,
			DiagnosticMessage: joinDiagnostic(parsed.diagnosticMessage, stderr),
			Score:             parsed.score,
		}, nil
	}

	status := model.StatusAccepted
	message := "Accepted"
	switch {
	case res.Status == "Accepted":
		status = model.StatusAccepted
	case res.ExitStatus == 1 || res.ExitStatus == 2:
		status = model.StatusWrongAnswer
		message = "Wrong Answer"
	default:
		status = model.StatusJudgementFailed
		message = role + " failed"
	}
	if status != model.StatusAccepted && stderr == "" {
		stderr = res.Status
	}
	return RunResult{
		Status:            status,
		TimeMS:            nsToMS(res.Time),
		MemoryKB:          bytesToKB(res.Memory),
		Message:           message,
		DiagnosticMessage: stderr,
	}, nil
}

type parsedResult struct {
	status            int
	score             *int
	message           string
	diagnosticMessage string
}

func parseResultJSON(raw string) (parsedResult, error) {
	var r resultJSON
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return parsedResult{}, err
	}
	status := strings.ToLower(strings.TrimSpace(r.Status))
	if status == "" {
		return parsedResult{}, fmt.Errorf("status is required")
	}
	out := parsedResult{
		score:             r.Score,
		message:           strings.TrimSpace(r.Message),
		diagnosticMessage: strings.TrimSpace(r.DiagnosticMessage),
	}
	switch status {
	case "accepted":
		out.status = model.StatusAccepted
	case "wrong_answer":
		out.status = model.StatusWrongAnswer
	case "partially_correct":
		out.status = model.StatusWrongAnswer
		if out.message == "" {
			out.message = "Partially Correct"
		}
	case "judgement_failed":
		out.status = model.StatusJudgementFailed
	case "invalid_interaction":
		out.status = model.StatusInvalidInteraction
	default:
		return parsedResult{}, fmt.Errorf("unsupported status %q", r.Status)
	}
	if out.message == "" {
		out.message = model.StatusText(out.status)
	}
	return out, nil
}

func renderArguments(template, fallback string, values map[string]string) ([]string, error) {
	if strings.TrimSpace(template) == "" {
		template = fallback
	}
	for key, value := range values {
		template = strings.ReplaceAll(template, "{"+key+"}", value)
	}
	if strings.TrimSpace(template) == "" {
		return nil, nil
	}
	return shlex.Split(template)
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
			Artifact:    "Main.jar",
			CompileArgs: []string{"/bin/sh", "-c", "javac Main.java && jar cf Main.jar *.class"},
			RunArgs:     []string{"/usr/bin/java", "-cp", "Main.jar", "Main"},
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
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}

func defaultEnv() []string {
	return []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "HOME=/tmp"}
}

func strPtr(s string) *string {
	return &s
}

func int64Ptr(v int64) *int64 {
	return &v
}

func nsToMS(ns uint64) int64 {
	return int64(ns / uint64(time.Millisecond))
}

func bytesToKB(b uint64) int64 {
	return int64((b + 1023) / 1024)
}

func programLimitsFromTask(task model.Task) programLimits {
	return programLimits{
		TimeMS:      task.TimeLimit,
		MemoryMB:    task.MemoryLimit,
		StackMB:     task.StackLimit,
		OutputBytes: 16 << 20,
		ProcLimit:   128,
	}
}

func programLimitsFromProgram(program model.Program) programLimits {
	return programLimits{
		TimeMS:      program.TimeLimit,
		MemoryMB:    program.MemoryLimit,
		StackMB:     program.StackLimit,
		OutputBytes: program.OutputLimit,
		ProcLimit:   128,
	}
}

func limitOrDefault(value, fallback int64) int64 {
	if value > 0 {
		return value
	}
	return fallback
}

func limitProc(value uint64) uint64 {
	if value > 0 {
		return value
	}
	return 128
}

func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func joinDiagnostic(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, "\n")
}
