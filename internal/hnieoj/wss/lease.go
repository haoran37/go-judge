package wss

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/criyle/go-judge/internal/hnieoj/protocol"
)

// Renew 通过 WSS 发送 LEASE_RENEW 并等待 LEASE_RENEWED，供 worker 续租接口使用。
// 只有当前会话 owner 且租约未过期时服务端才会续租；本方法不做授权判断。
func (c *Client) Renew(ctx context.Context, submissionID, judgeTaskID, attemptID string) (int64, error) {
	s := c.activeSession()
	if s == nil {
		return 0, ErrDisconnected
	}
	env, err := c.request(ctx, s, protocol.TypeLeaseRenew, LeasePayload{
		SubmissionID: submissionID,
		JudgeTaskID:  judgeTaskID,
		AttemptID:    attemptID,
	}, c.opts.ControlFrameBytes)
	if err != nil {
		return 0, err
	}
	if env.Type != protocol.TypeLeaseRenewed {
		return 0, fmt.Errorf("expected LEASE_RENEWED, got %s", env.Type)
	}
	var payload LeasePayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return 0, fmt.Errorf("malformed LEASE_RENEWED: %w", err)
	}
	if payload.SubmissionID != submissionID || payload.JudgeTaskID != judgeTaskID || payload.AttemptID != attemptID {
		return 0, errors.New("LEASE_RENEWED identity mismatch")
	}
	if payload.LeaseUntil <= 0 {
		return 0, errors.New("LEASE_RENEWED missing leaseUntil")
	}
	c.registry.Resume(submissionID, judgeTaskID, attemptID, payload.LeaseUntil, c.epoch.Load())
	return payload.LeaseUntil, nil
}
