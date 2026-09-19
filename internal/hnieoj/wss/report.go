package wss

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
)

// ReportStatusChanged/ReportCaseFinished 发送 TASK_RUNNING 进度并等待 TASK_EVENT_ACK。
func (c *Client) ReportStatusChanged(ctx context.Context, e model.Event) error {
	return c.reportProgress(ctx, e)
}

func (c *Client) ReportCaseFinished(ctx context.Context, e model.Event) error {
	return c.reportProgress(ctx, e)
}

// ReportJudgeFinished/ReportJudgeFailed 把终态写入持久结果队列；
// 真正投递由 resultLoop 在收到 TASK_RESULT_ACK 后才删除。
func (c *Client) ReportJudgeFinished(_ context.Context, e model.Event) error {
	return c.reportTerminal(e)
}

func (c *Client) ReportJudgeFailed(_ context.Context, e model.Event) error {
	return c.reportTerminal(e)
}

func (c *Client) reportProgress(ctx context.Context, e model.Event) error {
	s := c.activeSession()
	if s == nil {
		return ErrDisconnected
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(raw) > c.opts.TaskFrameBytes {
		return errFrameTooLarge
	}
	env, err := c.request(ctx, s, protocol.TypeTaskRunning, TaskEventPayload{
		SubmissionID: e.SubmissionID,
		Event:        raw,
	}, c.opts.TaskFrameBytes)
	if err != nil {
		return err
	}
	if env.Type != protocol.TypeTaskEventAck {
		return fmt.Errorf("expected TASK_EVENT_ACK, got %s", env.Type)
	}
	return nil
}

func (c *Client) reportTerminal(e model.Event) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return c.queueRecord(e.SubmissionID, e.JudgeTaskID, e.AttemptID, raw)
}

// QueueLen 返回尚未确认的终态结果数量。
func (c *Client) QueueLen() int {
	if c.queue == nil {
		return 0
	}
	return c.queue.Len()
}

// FlushResults 立即尝试投递一次队列中的结果（排空收尾用）。
func (c *Client) FlushResults(ctx context.Context) {
	s := c.activeSession()
	if s == nil {
		return
	}
	c.deliverResults(ctx, s)
}
