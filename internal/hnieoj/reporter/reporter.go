// Package reporter 定义判题事件上报接口。最终切到 WSS 后事件与终态都走节点通道，
// 不再存在 HTTP events 备用通路。
package reporter

import (
	"context"

	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
)

// Reporter 上报任务进度与终态。终态实现必须先持久化再返回 nil。
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
