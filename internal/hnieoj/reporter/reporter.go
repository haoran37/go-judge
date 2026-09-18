// Package reporter 向 HnieOJ 后端上报判题事件。
// 上报必须同时校验 HTTP 状态与 Result.code；HTTP 200 但业务 code != 200 视为失败。
package reporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
)

const maxResponseBytes = 1 << 20

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
	baseURL      string
	endpoint     string
	httpClient   *http.Client
	cred         Credential
	logger       logging.Logger
	maxRetries   int
	retryBackoff time.Duration
}

func NewHTTP(baseURL, endpoint string, httpClient *http.Client, cred Credential, logger logging.Logger, maxRetries int, retryBackoff time.Duration) *HTTPReporter {
	if maxRetries < 0 {
		maxRetries = 0
	}
	if retryBackoff <= 0 {
		retryBackoff = 2 * time.Second
	}
	return &HTTPReporter{
		baseURL:      strings.TrimRight(baseURL, "/"),
		endpoint:     endpoint,
		httpClient:   httpClient,
		cred:         cred,
		logger:       logger,
		maxRetries:   maxRetries,
		retryBackoff: retryBackoff,
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

// report 对可恢复的传输/5xx 错误在租约有效期内有限重试；
// 业务失败（HTTP 200 但 Result.code != 200）不重试。终态事件重报由后端指纹幂等保证。
func (r *HTTPReporter) report(ctx context.Context, e model.Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	endpoint := strings.ReplaceAll(r.endpoint, "{submissionId}", e.SubmissionID)
	url := r.baseURL + endpoint
	attempts := r.maxRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		retryable, err := r.post(ctx, url, body, e)
		if err == nil {
			r.logger.Info("report succeeded", logging.String("submissionId", e.SubmissionID), logging.String("eventType", e.EventType))
			return nil
		}
		if !retryable || attempt == attempts-1 || ctx.Err() != nil {
			return err
		}
		timer := time.NewTimer(r.retryBackoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// post 返回 (retryable, error)。retryable 仅对网络错误与 HTTP 5xx 为 true。
func (r *HTTPReporter) post(ctx context.Context, url string, body []byte, e model.Event) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("%s:%s:%s:%d:%d", e.SubmissionID, e.JudgeTaskID, e.EventType, e.JudgedCase, e.CurrentCase))
	r.cred.Apply(req)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		r.logger.Warn("report transport failed", logging.String("submissionId", e.SubmissionID), logging.String("eventType", e.EventType), logging.Error(err))
		return true, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return true, err
	}
	if int64(len(raw)) > maxResponseBytes {
		return false, errors.New("report response body too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("report status %d", resp.StatusCode)
		r.logger.Warn("report failed", logging.String("submissionId", e.SubmissionID), logging.String("eventType", e.EventType), logging.Error(err))
		return resp.StatusCode >= 500, err
	}
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return false, fmt.Errorf("malformed report response: %w", err)
	}
	if envelope.Code != http.StatusOK {
		err := fmt.Errorf("report result code %d: %s", envelope.Code, envelope.Msg)
		r.logger.Warn("report rejected", logging.String("submissionId", e.SubmissionID), logging.String("eventType", e.EventType), logging.Error(err))
		return false, err
	}
	return false, nil
}
