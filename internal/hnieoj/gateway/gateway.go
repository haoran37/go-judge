// Package gateway 封装判题节点访问后端内嵌网关的 HTTPS 任务协议：
// 有界单任务领取与租约续期。所有请求同时校验 HTTP 状态与 Result.code。
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
)

const maxResponseBytes = 1 << 20

// Credential 是网关请求所需的逐节点凭证（*auth.Credential 满足该接口）。
type Credential interface {
	Apply(req *http.Request)
	Expired(now time.Time) bool
}

// Claim 是一次成功领取的任务及其租约元数据。
type Claim struct {
	Task             model.Task
	AttemptID        string
	LeaseUntil       int64
	RenewAfterMillis int
}

type claimPayload struct {
	Task             model.Task `json:"task"`
	AttemptID        string     `json:"attemptId"`
	LeaseUntil       int64      `json:"leaseUntil"`
	RenewAfterMillis int        `json:"renewAfterMillis"`
}

type leasePayload struct {
	LeaseUntil int64 `json:"leaseUntil"`
}

type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 是后端任务网关的有界 HTTP 客户端。
type Client struct {
	baseURL    string
	httpClient *http.Client
	cred       Credential
	logger     logging.Logger
}

func New(baseURL string, httpClient *http.Client, cred Credential, logger logging.Logger) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
		cred:       cred,
		logger:     logger,
	}
}

// Claim 至多领取一个任务；无任务或额度已满返回 (nil, nil)。
func (c *Client) Claim(ctx context.Context) (*Claim, error) {
	if c.cred.Expired(time.Now()) {
		return nil, errors.New("judge node credential expired")
	}
	raw, err := c.post(ctx, c.baseURL+"/judge/tasks/claim", []byte("{}"))
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("malformed claim response: %w", err)
	}
	if err := checkEnvelope(env); err != nil {
		return nil, err
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, nil
	}
	var payload claimPayload
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		return nil, fmt.Errorf("malformed claim payload: %w", err)
	}
	if strings.TrimSpace(payload.AttemptID) == "" || payload.Task.SubmissionID == "" || payload.Task.JudgeTaskID == "" {
		return nil, errors.New("claim payload missing attemptId/submissionId/judgeTaskId")
	}
	if payload.LeaseUntil <= 0 {
		return nil, errors.New("claim payload missing leaseUntil")
	}
	payload.Task.AttemptID = payload.AttemptID
	return &Claim{
		Task:             payload.Task,
		AttemptID:        payload.AttemptID,
		LeaseUntil:       payload.LeaseUntil,
		RenewAfterMillis: payload.RenewAfterMillis,
	}, nil
}

// Renew 续租当前任务，返回新的租约到期时间（Unix 毫秒）。
func (c *Client) Renew(ctx context.Context, submissionID, judgeTaskID, attemptID string) (int64, error) {
	if c.cred.Expired(time.Now()) {
		return 0, errors.New("judge node credential expired")
	}
	body, err := json.Marshal(struct {
		JudgeTaskID string `json:"judgeTaskId"`
		AttemptID   string `json:"attemptId"`
	}{JudgeTaskID: judgeTaskID, AttemptID: attemptID})
	if err != nil {
		return 0, err
	}
	endpoint := c.baseURL + "/judge/tasks/" + url.PathEscape(submissionID) + "/lease"
	raw, err := c.post(ctx, endpoint, body)
	if err != nil {
		return 0, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, fmt.Errorf("malformed lease response: %w", err)
	}
	if err := checkEnvelope(env); err != nil {
		return 0, err
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return 0, errors.New("lease response payload is empty")
	}
	var payload leasePayload
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		return 0, fmt.Errorf("malformed lease payload: %w", err)
	}
	if payload.LeaseUntil <= 0 {
		return 0, errors.New("lease response missing leaseUntil")
	}
	return payload.LeaseUntil, nil
}

func (c *Client) post(ctx context.Context, endpoint string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.cred.Apply(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxResponseBytes {
		return nil, errors.New("backend response body too large")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, &auth.DeniedError{StatusCode: resp.StatusCode, Msg: strings.TrimSpace(string(raw))}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("backend returned http %d", resp.StatusCode)
	}
	return raw, nil
}

func checkEnvelope(env envelope) error {
	if env.Code == http.StatusUnauthorized || env.Code == http.StatusForbidden {
		return &auth.DeniedError{Code: env.Code, Msg: env.Msg}
	}
	if env.Code != http.StatusOK {
		return fmt.Errorf("backend result code %d: %s", env.Code, env.Msg)
	}
	return nil
}
