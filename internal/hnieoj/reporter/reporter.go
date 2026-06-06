package reporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
)

type Credential interface {
	Apply(req *http.Request)
}

type Reporter interface {
	ReportStatusChanged(context.Context, model.Event) error
	ReportCaseFinished(context.Context, model.Event) error
	ReportJudgeFinished(context.Context, model.Event) error
	ReportJudgeFailed(context.Context, model.Event) error
}

type LogReporter struct {
	logger logging.Logger
}

func NewLog(logger logging.Logger) *LogReporter {
	return &LogReporter{logger: logger}
}

func (r *LogReporter) ReportStatusChanged(_ context.Context, e model.Event) error {
	r.log("status changed", e)
	return nil
}

func (r *LogReporter) ReportCaseFinished(_ context.Context, e model.Event) error {
	r.log("case finished", e)
	return nil
}

func (r *LogReporter) ReportJudgeFinished(_ context.Context, e model.Event) error {
	r.log("judge finished", e)
	return nil
}

func (r *LogReporter) ReportJudgeFailed(_ context.Context, e model.Event) error {
	r.log("judge failed", e)
	return nil
}

func (r *LogReporter) log(message string, e model.Event) {
	r.logger.Info(message,
		logging.String("submissionId", e.SubmissionID),
		logging.String("eventType", e.EventType),
		logging.Int("status", e.Status),
		logging.String("message", e.Message))
}

type HTTPReporter struct {
	baseURL    string
	endpoint   string
	httpClient *http.Client
	cred       Credential
	logger     logging.Logger
}

func NewHTTP(baseURL, endpoint string, httpClient *http.Client, cred Credential, logger logging.Logger) *HTTPReporter {
	return &HTTPReporter{
		baseURL:    strings.TrimRight(baseURL, "/"),
		endpoint:   endpoint,
		httpClient: httpClient,
		cred:       cred,
		logger:     logger,
	}
}

func (r *HTTPReporter) ReportStatusChanged(ctx context.Context, e model.Event) error {
	return r.report(ctx, e)
}

func (r *HTTPReporter) ReportCaseFinished(ctx context.Context, e model.Event) error {
	return r.report(ctx, e)
}

func (r *HTTPReporter) ReportJudgeFinished(ctx context.Context, e model.Event) error {
	return r.report(ctx, e)
}

func (r *HTTPReporter) ReportJudgeFailed(ctx context.Context, e model.Event) error {
	return r.report(ctx, e)
}

func (r *HTTPReporter) report(ctx context.Context, e model.Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	endpoint := strings.ReplaceAll(r.endpoint, "{submissionId}", e.SubmissionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("%s:%s:%d:%d", e.SubmissionID, e.EventType, e.JudgedCase, e.CurrentCase))
	r.cred.Apply(req)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		r.logger.Warn("report failed", logging.String("submissionId", e.SubmissionID), logging.String("eventType", e.EventType), logging.Error(err))
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("report status %d", resp.StatusCode)
		r.logger.Warn("report failed", logging.String("submissionId", e.SubmissionID), logging.String("eventType", e.EventType), logging.Error(err))
		return err
	}
	r.logger.Info("report succeeded", logging.String("submissionId", e.SubmissionID), logging.String("eventType", e.EventType))
	return nil
}
