package processor

import (
	"context"
	"fmt"
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
	inFlight       sync.Map
}

func New(testdataClient *testdata.Client, runnerClient *runner.Client, reporter reporter.Reporter, cred *auth.Credential, logger logging.Logger) *Processor {
	return &Processor{
		testdataClient: testdataClient,
		runnerClient:   runnerClient,
		reporter:       reporter,
		cred:           cred,
		logger:         logger,
	}
}

func (p *Processor) Process(ctx context.Context, task model.Task) error {
	if task.SubmissionID == "" {
		return ErrNonRetryable{Err: fmt.Errorf("submissionId is required")}
	}
	if p.cred.Expired(time.Now()) {
		return ErrRetryable{Err: fmt.Errorf("temporary credential expired")}
	}
	if _, loaded := p.inFlight.LoadOrStore(task.SubmissionID, struct{}{}); loaded {
		p.logger.Warn("duplicate task ignored", logging.String("submissionId", task.SubmissionID))
		return nil
	}
	defer p.inFlight.Delete(task.SubmissionID)

	p.logger.Info("task received", logging.String("submissionId", task.SubmissionID), logging.Int64("problemId", task.ProblemID), logging.String("language", task.Language))
	if task.JudgeMode != "" && task.JudgeMode != "default" {
		err := ErrNonRetryable{Err: fmt.Errorf("unsupported judge mode %q", task.JudgeMode)}
		_ = p.reportFailed(ctx, task, 0, err.Error())
		return err
	}

	cases, _, err := p.testdataClient.Ensure(ctx, task.ProblemID, task.DataVersion)
	if err != nil {
		return ErrRetryable{Err: err}
	}
	total := len(cases)

	if err := p.reporter.ReportStatusChanged(ctx, model.NewEvent(model.EventStatusChanged, task, model.StatusCompiling, total, 0, 0, nil, "Compiling")); err != nil {
		return ErrRetryable{Err: err}
	}
	compileResult, err := p.runnerClient.Compile(ctx, task)
	if err != nil {
		return ErrRetryable{Err: err}
	}
	if compileResult.Status != model.StatusAccepted {
		event := model.NewEvent(model.EventJudgeFailed, task, compileResult.Status, total, 0, 0, nil, compileResult.Message)
		if err := p.reporter.ReportJudgeFailed(ctx, event); err != nil {
			return ErrRetryable{Err: err}
		}
		return nil
	}

	if err := p.reporter.ReportStatusChanged(ctx, model.NewEvent(model.EventStatusChanged, task, model.StatusRunning, total, 0, 1, nil, "Running")); err != nil {
		return ErrRetryable{Err: err}
	}

	results := make([]model.CaseResult, 0, total)
	finalStatus := model.StatusAccepted
	totalScore := 0
	for i, tc := range cases {
		runResult, err := p.runnerClient.RunCase(ctx, task, tc, compileResult.ArtifactFileID)
		if err != nil {
			return ErrRetryable{Err: err}
		}
		score := caseScore(task, total, runResult.Status)
		if task.ProblemType == model.ProblemTypeACM && runResult.Status != model.StatusAccepted {
			score = 0
		}
		cr := model.CaseResult{
			CaseID:     tc.ID,
			Status:     runResult.Status,
			StatusText: model.StatusText(runResult.Status),
			Time:       runResult.TimeMS,
			Memory:     runResult.MemoryKB,
			Score:      score,
			UserOutput: runResult.UserOutput,
		}
		results = append(results, cr)
		if runResult.Status != model.StatusAccepted && finalStatus == model.StatusAccepted {
			finalStatus = runResult.Status
		}
		totalScore += score
		message := fmt.Sprintf("Test %d/%d %s", i+1, total, cr.StatusText)
		event := model.NewEvent(model.EventCaseFinished, task, model.StatusRunning, total, i+1, nextCase(i+1, total), &cr, message)
		if err := p.reporter.ReportCaseFinished(ctx, event); err != nil {
			return ErrRetryable{Err: err}
		}
		p.logger.Info("case finished", logging.String("submissionId", task.SubmissionID), logging.String("caseId", tc.ID), logging.Int("status", cr.Status))
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
	_ = results
	return nil
}

func (p *Processor) reportFailed(ctx context.Context, task model.Task, total int, message string) error {
	event := model.NewEvent(model.EventJudgeFailed, task, model.StatusSystemError, total, 0, 0, nil, message)
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

func caseScore(task model.Task, total int, status int) int {
	if status != model.StatusAccepted || total <= 0 {
		return 0
	}
	if task.IOScore <= 0 {
		return 0
	}
	return task.IOScore / total
}

func nextCase(judged, total int) int {
	if judged >= total {
		return total
	}
	return judged + 1
}
