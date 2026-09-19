// Package wss 实现合同规定的 WSS 节点通道：单连接认证/续授权、bounded 单写者、
// 单 reader 分发、requestId 关联、RESUME 恢复、任务/租约/心跳、结果确认。
// HTTPS 只负责入网、签名测试数据下载与密钥轮换。
package wss

import (
	"encoding/json"

	"github.com/criyle/go-judge/internal/hnieoj/model"
)

// ChallengePayload 是 AUTH_CHALLENGE payload。
type ChallengePayload struct {
	ChallengeID string `json:"challengeId"`
	Nonce       string `json:"nonce"`
	Audience    string `json:"audience"`
	ExpiresAt   int64  `json:"expiresAt"`
	ServerTime  int64  `json:"serverTime"`
}

// AuthResponsePayload 是 AUTH_RESPONSE payload。
type AuthResponsePayload struct {
	NodeID      string `json:"nodeId"`
	KeyID       string `json:"keyId"`
	ChallengeID string `json:"challengeId"`
	Signature   string `json:"signature"`
}

// AuthOKPayload 是 AUTH_OK payload。
type AuthOKPayload struct {
	NodeID                  string   `json:"nodeId"`
	KeyID                   string   `json:"keyId"`
	AccessToken             string   `json:"accessToken"`
	AccessExpiresAt         int64    `json:"accessExpiresAt"`
	SessionEpoch            int64    `json:"sessionEpoch"`
	ServerTime              int64    `json:"serverTime"`
	MaxConcurrency          int      `json:"maxConcurrency"`
	SupportedJudgeModes     []string `json:"supportedJudgeModes"`
	AuthorizationUntil      int64    `json:"authorizationUntil"`
	HeartbeatIntervalMillis int      `json:"heartbeatIntervalMillis"`
}

// ResumeAttempt 是 RESUME_TASKS 中的一个 attempt 标识。
type ResumeAttempt struct {
	SubmissionID string `json:"submissionId"`
	JudgeTaskID  string `json:"judgeTaskId"`
	AttemptID    string `json:"attemptId"`
}

// ResumeTasksPayload 是 RESUME_TASKS payload。
type ResumeTasksPayload struct {
	Attempts []ResumeAttempt `json:"attempts"`
}

// ResumedTask 是 RESUME_RESULT 中恢复/已完成的任务。
type ResumedTask struct {
	SubmissionID     string `json:"submissionId"`
	JudgeTaskID      string `json:"judgeTaskId"`
	AttemptID        string `json:"attemptId"`
	LeaseUntil       int64  `json:"leaseUntil"`
	RenewAfterMillis int    `json:"renewAfterMillis"`
	Completed        bool   `json:"completed,omitempty"`
}

// RejectedTask 是 RESUME_RESULT 中拒绝的任务。
type RejectedTask struct {
	SubmissionID string `json:"submissionId"`
	JudgeTaskID  string `json:"judgeTaskId"`
	AttemptID    string `json:"attemptId"`
	Reason       string `json:"reason"`
}

// ResumeResultPayload 是 RESUME_RESULT payload。
type ResumeResultPayload struct {
	Resumed  []ResumedTask  `json:"resumed"`
	Rejected []RejectedTask `json:"rejected"`
}

// ReadyPayload 是 READY payload。
type ReadyPayload struct {
	AvailableSlots int `json:"availableSlots"`
}

// TaskAssignPayload 是 TASK_ASSIGN payload。
type TaskAssignPayload struct {
	Task             model.Task `json:"task"`
	AttemptID        string     `json:"attemptId"`
	LeaseUntil       int64      `json:"leaseUntil"`
	RenewAfterMillis int        `json:"renewAfterMillis"`
}

// TaskAckPayload 是 TASK_ACK payload（只确认收到，不是 Stream ACK）。
type TaskAckPayload struct {
	SubmissionID string `json:"submissionId"`
	JudgeTaskID  string `json:"judgeTaskId"`
	AttemptID    string `json:"attemptId"`
}

// TaskEventPayload 是 TASK_RUNNING / TASK_RESULT 的 payload。
// event 是既有 JudgeResultEventRequest，包含 judgeTaskId/attemptId。
type TaskEventPayload struct {
	SubmissionID string          `json:"submissionId"`
	Event        json.RawMessage `json:"event"`
}

// TaskResultAckPayload 是 TASK_RESULT_ACK payload。
type TaskResultAckPayload struct {
	SubmissionID string `json:"submissionId"`
	JudgeTaskID  string `json:"judgeTaskId"`
	AttemptID    string `json:"attemptId"`
}

// LeasePayload 是 LEASE_RENEW / LEASE_RENEWED payload。
type LeasePayload struct {
	SubmissionID string `json:"submissionId"`
	JudgeTaskID  string `json:"judgeTaskId"`
	AttemptID    string `json:"attemptId"`
	LeaseUntil   int64  `json:"leaseUntil,omitempty"`
}

// TaskCancelItem 标识一个待取消任务。
type TaskCancelItem struct {
	SubmissionID string `json:"submissionId"`
	JudgeTaskID  string `json:"judgeTaskId"`
	AttemptID    string `json:"attemptId"`
}

// TaskCancelPayload 是 TASK_CANCEL payload。
type TaskCancelPayload struct {
	Tasks  []TaskCancelItem `json:"tasks"`
	Reason string           `json:"reason"`
}

// NodeStatePayload 是 NODE_STATE payload。
type NodeStatePayload struct {
	Status   string `json:"status"`
	Draining bool   `json:"draining"`
}

// ErrorPayload 是 ERROR payload；auth denied/epoch stale 不可重试。
type ErrorPayload struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// HeartbeatAckPayload 是 HEARTBEAT_ACK payload（可空）。
type HeartbeatAckPayload struct {
	ServerTime int64 `json:"serverTime,omitempty"`
}
