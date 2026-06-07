package processor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/reporter"
	"github.com/criyle/go-judge/internal/hnieoj/runner"
	"github.com/criyle/go-judge/internal/hnieoj/testdata"
)

type Processor struct {
	testdataClient *testdata.Client
	runnerClient   *runner.Client
	reporter       reporter.Reporter
	cred           *auth.Credential
	logger         logging.Logger
	supportedModes map[string]struct{}
	inFlight       sync.Map
}

func New(testdataClient *testdata.Client, runnerClient *runner.Client, reporter reporter.Reporter, cred *auth.Credential, logger logging.Logger, supportedModes []string) *Processor {
	return &Processor{
		testdataClient: testdataClient,
		runnerClient:   runnerClient,
		reporter:       reporter,
		cred:           cred,
		logger:         logger,
		supportedModes: supportedModeSet(supportedModes),
	}
}

func (p *Processor) Process(ctx context.Context, task model.Task) error {
	if task.SubmissionID == "" {
		return ErrNonRetryable{Err: fmt.Errorf("submissionId is required")}
	}
	if task.JudgeTaskID == "" {
		return ErrNonRetryable{Err: fmt.Errorf("judgeTaskId is required")}
	}
	if p.cred.Expired(time.Now()) {
		return ErrRetryable{Err: fmt.Errorf("temporary credential expired")}
	}
	key := taskKey(task)
	if _, loaded := p.inFlight.LoadOrStore(key, struct{}{}); loaded {
		p.logger.Warn("duplicate task ignored", logging.String("submissionId", task.SubmissionID), logging.String("judgeTaskId", task.JudgeTaskID))
		return nil
	}
	defer p.inFlight.Delete(key)

	mode := normalizeMode(task.JudgeMode)
	p.logger.Info("task received", logging.String("submissionId", task.SubmissionID), logging.String("judgeTaskId", task.JudgeTaskID), logging.Int64("problemId", task.ProblemID), logging.String("language", task.Language), logging.String("judgeMode", mode))
	if !p.supports(mode) {
		err := ErrNonRetryable{Err: fmt.Errorf("unsupported judge mode %q", mode)}
		_ = p.reportFailed(ctx, task, 0, model.StatusSystemError, err.Error(), "")
		return err
	}

	cases, _, err := p.testdataClient.Ensure(ctx, task.ProblemID, task.DataVersion)
	if err != nil {
		var permanent testdata.ErrPermanent
		if errors.As(err, &permanent) {
			_ = p.reportFailed(ctx, task, 0, model.StatusSystemError, permanent.Error(), "")
			return ErrNonRetryable{Err: permanent}
		}
		return ErrRetryable{Err: err}
	}
	total := len(cases)

	if err := p.reporter.ReportStatusChanged(ctx, model.NewEvent(model.EventStatusChanged, task, model.StatusCompiling, total, 0, 0, nil, "Compiling")); err != nil {
		return ErrRetryable{Err: err}
	}

	userCompile, err := p.runnerClient.Compile(ctx, task)
	if err != nil {
		return ErrRetryable{Err: err}
	}
	if userCompile.ArtifactFileID != "" {
		defer p.deleteArtifact(userCompile.ArtifactFileID)
	}
	if userCompile.Status != model.StatusAccepted {
		if err := p.reportFailed(ctx, task, total, userCompile.Status, userCompile.Message, ""); err != nil {
			return ErrRetryable{Err: err}
		}
		return nil
	}

	var checkerCompile runner.CompileResult
	var interactorCompile runner.CompileResult
	switch mode {
	case model.JudgeModeSPJ:
		if task.Checker == nil {
			if err := p.reportFailed(ctx, task, total, model.StatusJudgementFailed, "checker is required", ""); err != nil {
				return ErrRetryable{Err: err}
			}
			return nil
		}
		checkerCompile, err = p.runnerClient.CompileProgram(ctx, task.SubmissionID+":checker:compile", task.Checker.Language, task.Checker.Source)
		if err != nil {
			return ErrRetryable{Err: err}
		}
		if checkerCompile.ArtifactFileID != "" {
			defer p.deleteArtifact(checkerCompile.ArtifactFileID)
		}
		if checkerCompile.Status != model.StatusAccepted {
			if err := p.reportFailed(ctx, task, total, model.StatusJudgementFailed, "checker compile failed", checkerCompile.Message); err != nil {
				return ErrRetryable{Err: err}
			}
			return nil
		}
	case model.JudgeModeInteractive:
		if task.Interactor == nil {
			if err := p.reportFailed(ctx, task, total, model.StatusInvalidInteraction, "interactor is required", ""); err != nil {
				return ErrRetryable{Err: err}
			}
			return nil
		}
		interactorCompile, err = p.runnerClient.CompileProgram(ctx, task.SubmissionID+":interactor:compile", task.Interactor.Language, task.Interactor.Source)
		if err != nil {
			return ErrRetryable{Err: err}
		}
		if interactorCompile.ArtifactFileID != "" {
			defer p.deleteArtifact(interactorCompile.ArtifactFileID)
		}
		if interactorCompile.Status != model.StatusAccepted {
			if err := p.reportFailed(ctx, task, total, model.StatusInvalidInteraction, "interactor compile failed", interactorCompile.Message); err != nil {
				return ErrRetryable{Err: err}
			}
			return nil
		}
	}

	if err := p.reporter.ReportStatusChanged(ctx, model.NewEvent(model.EventStatusChanged, task, model.StatusRunning, total, 0, 1, nil, "Running")); err != nil {
		return ErrRetryable{Err: err}
	}

	finalStatus := model.StatusAccepted
	totalScore := 0
	for i, tc := range cases {
		runResult, err := p.runCase(ctx, mode, task, tc, userCompile.ArtifactFileID, checkerCompile.ArtifactFileID, interactorCompile.ArtifactFileID)
		if err != nil {
			return ErrRetryable{Err: err}
		}
		score := p.caseScore(task, total, i, runResult)
		cr := model.CaseResult{
			CaseID:            tc.ID,
			Status:            runResult.Status,
			StatusText:        model.StatusText(runResult.Status),
			Time:              runResult.TimeMS,
			Memory:            runResult.MemoryKB,
			Score:             score,
			UserOutput:        runResult.UserOutput,
			DiagnosticMessage: runResult.DiagnosticMessage,
		}
		if runResult.Status != model.StatusAccepted && finalStatus == model.StatusAccepted {
			finalStatus = runResult.Status
		}
		totalScore += score
		message := fmt.Sprintf("Test %d/%d %s", i+1, total, cr.StatusText)
		if strings.TrimSpace(runResult.Message) != "" {
			message = fmt.Sprintf("%s: %s", message, strings.TrimSpace(runResult.Message))
		}
		event := model.NewEvent(model.EventCaseFinished, task, model.StatusRunning, total, i+1, nextCase(i+1, total), &cr, message)
		event.DiagnosticMessage = runResult.DiagnosticMessage
		if err := p.reporter.ReportCaseFinished(ctx, event); err != nil {
			return ErrRetryable{Err: err}
		}
		p.logger.Info("case finished", logging.String("submissionId", task.SubmissionID), logging.String("caseId", tc.ID), logging.Int("status", cr.Status), logging.Int("score", cr.Score))
	}

	if task.ProblemType == model.ProblemTypeACM && finalStatus == model.StatusAccepted {
		totalScore = task.IOScore
	}
	if task.ProblemType == model.ProblemTypeOI && totalScore >= task.IOScore {
		finalStatus = model.StatusAccepted
	}
	message := fmt.Sprintf("Judge finished: %s", model.StatusText(finalStatus))
	event := model.NewEvent(model.EventJudgeFinished, task, finalStatus, total, total, total, nil, message)
	event.Score = totalScore
	if err := p.reporter.ReportJudgeFinished(ctx, event); err != nil {
		return ErrRetryable{Err: err}
	}
	p.logger.Info("judge finished", logging.String("submissionId", task.SubmissionID), logging.Int("status", finalStatus), logging.Int("score", totalScore))
	return nil
}

func (p *Processor) runCase(ctx context.Context, mode string, task model.Task, tc model.Case, userArtifactID, checkerArtifactID, interactorArtifactID string) (runner.RunResult, error) {
	switch mode {
	case model.JudgeModeSPJ:
		return p.runnerClient.RunSPJCase(ctx, task, tc, userArtifactID, checkerArtifactID)
	case model.JudgeModeInteractive:
		return p.runnerClient.RunInteractiveCase(ctx, task, tc, userArtifactID, interactorArtifactID)
	default:
		return p.runnerClient.RunCase(ctx, task, tc, userArtifactID)
	}
}

func (p *Processor) caseScore(task model.Task, total, caseIndex int, runResult runner.RunResult) int {
	if runResult.Score != nil {
		return clamp(*runResult.Score, 0, caseMaxScore(task, total, caseIndex))
	}
	score := caseScore(task, total, caseIndex, runResult.Status)
	if task.ProblemType == model.ProblemTypeACM && runResult.Status != model.StatusAccepted {
		return 0
	}
	return score
}

func (p *Processor) deleteArtifact(fileID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.runnerClient.DeleteFile(ctx, fileID); err != nil {
		p.logger.Warn("artifact cleanup failed", logging.String("fileId", fileID), logging.Error(err))
	}
}

func taskKey(task model.Task) string {
	return task.SubmissionID + ":" + task.JudgeTaskID
}

func (p *Processor) reportFailed(ctx context.Context, task model.Task, total int, status int, message, diagnostic string) error {
	event := model.NewEvent(model.EventJudgeFailed, task, status, total, 0, 0, nil, message)
	event.DiagnosticMessage = diagnostic
	return p.reporter.ReportJudgeFailed(ctx, event)
}

type ErrRetryable struct {
	Err error
}

func (e ErrRetryable) Error() string {
	return e.Err.Error()
}

func (e ErrRetryable) Unwrap() error {
	return e.Err
}

type ErrNonRetryable struct {
	Err error
}

func (e ErrNonRetryable) Error() string {
	return e.Err.Error()
}

func (e ErrNonRetryable) Unwrap() error {
	return e.Err
}

func caseScore(task model.Task, total, caseIndex int, status int) int {
	if status != model.StatusAccepted || total <= 0 {
		return 0
	}
	if task.IOScore <= 0 {
		return 0
	}
	return caseMaxScore(task, total, caseIndex)
}

func caseMaxScore(task model.Task, total, caseIndex int) int {
	if total <= 0 || task.IOScore <= 0 {
		return 0
	}
	base := task.IOScore / total
	if caseIndex < task.IOScore%total {
		return base + 1
	}
	return base
}

func nextCase(judged, total int) int {
	if judged >= total {
		return total
	}
	return judged + 1
}

func normalizeMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return model.JudgeModeDefault
	}
	return mode
}

func supportedModeSet(modes []string) map[string]struct{} {
	if len(modes) == 0 {
		modes = []string{model.JudgeModeDefault}
	}
	out := make(map[string]struct{}, len(modes))
	for _, mode := range modes {
		out[normalizeMode(mode)] = struct{}{}
	}
	return out
}

func (p *Processor) supports(mode string) bool {
	_, ok := p.supportedModes[normalizeMode(mode)]
	return ok
}

func clamp(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
