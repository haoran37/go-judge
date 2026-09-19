// Package enrollment 实现统一 Bootstrap 入网：一次性明文 bootstrap 换取
// 服务端生成的 nodeId/keyId。注册前身份（keypair + enrollmentId）已由 identity 原子持久化，
// 注册回复丢失时用同一 enrollmentId + 公钥重试可恢复同一 node，绝不创建第二个 node。
package enrollment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/identity"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
)

const maxResponseBytes = 1 << 20

// Bootstrap 描述一次性入网凭证来源。
type Bootstrap struct {
	// TokenFile 是保存明文 bootstrap 的 0600 文件；注册成功后删除。
	TokenFile string
	// Token 是内联 bootstrap（仅测试/兜底；不写日志）。
	Token string
}

// Read 返回当前 bootstrap 明文；没有则返回空串。
func (b Bootstrap) Read() (string, error) {
	if token := strings.TrimSpace(b.Token); token != "" {
		return token, nil
	}
	path := strings.TrimSpace(b.TokenFile)
	if path == "" {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// Consume 删除 bootstrap 文件（若存在）。
func (b Bootstrap) Consume() error {
	path := strings.TrimSpace(b.TokenFile)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Challenge 是服务端签发的登记挑战。
type Challenge struct {
	ChallengeID string `json:"challengeId"`
	Nonce       string `json:"nonce"`
	Audience    string `json:"audience"`
	ExpiresAt   int64  `json:"expiresAt"`
	ServerTime  int64  `json:"serverTime"`
}

// Identity 是服务端返回的非敏感身份。
type Identity struct {
	NodeID              string   `json:"nodeId"`
	KeyID               string   `json:"keyId"`
	NodeType            string   `json:"nodeType"`
	AuthorizationUntil  int64    `json:"authorizationUntil"`
	MaxConcurrency      int      `json:"maxConcurrency"`
	SupportedJudgeModes []string `json:"supportedJudgeModes"`
}

type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 是登记 HTTP 客户端。
type Client struct {
	baseURL    string
	nodeName   string
	httpClient *http.Client
	store      *identity.Store
	bootstrap  Bootstrap
	logger     logging.Logger
	now        func() time.Time
}

func New(baseURL, nodeName string, store *identity.Store, bootstrap Bootstrap, httpClient *http.Client, logger logging.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	// 已持久化过 nodeName 的身份必须复用它，避免注册回复丢失后重试绑定到不同 nodeName。
	if store != nil {
		if persisted := strings.TrimSpace(store.NodeName()); persisted != "" {
			nodeName = persisted
		}
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		nodeName:   nodeName,
		httpClient: httpClient,
		store:      store,
		bootstrap:  bootstrap,
		logger:     logger,
		now:        time.Now,
	}
}

// SetNow 允许测试注入时钟。
func (c *Client) SetNow(now func() time.Time) { c.now = now }

// Enrolled 表示本地已具备服务端分配的身份。
func (c *Client) Enrolled() bool {
	return c.store.NodeID() != "" && c.store.CurrentKeyID() != ""
}

// Enroll 完成一次登记（challenge + enroll）。失败可安全重试：
// 重新申请挑战并用同一 enrollmentId/公钥注册，服务端会返回同一身份。
func (c *Client) Enroll(ctx context.Context) (Identity, error) {
	bootstrapToken, err := c.bootstrap.Read()
	if err != nil {
		return Identity{}, fmt.Errorf("read bootstrap: %w", err)
	}
	if bootstrapToken == "" {
		return Identity{}, errors.New("bootstrap token is required for enrollment")
	}
	challenge, err := c.requestChallenge(ctx, bootstrapToken)
	if err != nil {
		return Identity{}, err
	}
	identity, err := c.submit(ctx, bootstrapToken, challenge)
	if err != nil {
		return Identity{}, err
	}
	if err := c.store.ApplyEnrollment(identity.NodeID, identity.KeyID); err != nil {
		return Identity{}, fmt.Errorf("persist enrollment: %w", err)
	}
	if err := c.bootstrap.Consume(); err != nil {
		c.logger.Warn("consume bootstrap token failed", logging.Error(err))
	}
	c.logger.Info("node enrolled",
		logging.String("nodeId", identity.NodeID),
		logging.String("keyId", identity.KeyID),
		logging.String("nodeType", identity.NodeType))
	return identity, nil
}

// EnsureEnrolled 在需要时登记；已登记则直接返回。transient 错误按退避重试，
// 明确拒绝立即返回，绝不自动更换身份绕过。
func (c *Client) EnsureEnrolled(ctx context.Context, retryBackoff time.Duration) error {
	if c.Enrolled() {
		return nil
	}
	if retryBackoff <= 0 {
		retryBackoff = 10 * time.Second
	}
	backoff := retryBackoff
	for {
		_, err := c.Enroll(ctx)
		if err == nil {
			return nil
		}
		if auth.IsDenied(err) || auth.IsPermanent(err) {
			return err
		}
		c.logger.Warn("enrollment failed, will retry", logging.Error(err))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 2*time.Minute {
			backoff *= 2
		}
	}
}

func (c *Client) requestChallenge(ctx context.Context, bootstrapToken string) (Challenge, error) {
	body, err := json.Marshal(map[string]string{
		"bootstrapToken": bootstrapToken,
		"enrollmentId":   c.store.EnrollmentID(),
		"nodeName":       c.nodeName,
		"publicKey":      c.store.PublicKey(),
	})
	if err != nil {
		return Challenge{}, err
	}
	raw, err := c.post(ctx, "/judge/nodes/enrollment-challenges", body)
	if err != nil {
		return Challenge{}, err
	}
	var challenge Challenge
	if err := decodeData(raw, &challenge); err != nil {
		return Challenge{}, fmt.Errorf("malformed challenge response: %w", err)
	}
	if challenge.ChallengeID == "" || challenge.Nonce == "" || challenge.Audience == "" {
		return Challenge{}, errors.New("challenge response missing challengeId/nonce/audience")
	}
	if challenge.ExpiresAt > 0 && c.now().UnixMilli() >= challenge.ExpiresAt {
		return Challenge{}, errors.New("challenge already expired")
	}
	return challenge, nil
}

func (c *Client) submit(ctx context.Context, bootstrapToken string, challenge Challenge) (Identity, error) {
	digest := protocol.BootstrapDigest(bootstrapToken)
	fields := protocol.EnrollFields(
		challenge.Audience, challenge.ChallengeID, challenge.Nonce,
		c.store.EnrollmentID(), c.nodeName, c.store.PublicKey(), digest,
	)
	signature, err := c.store.Sign(fields...)
	if err != nil {
		return Identity{}, fmt.Errorf("sign enrollment proof: %w", err)
	}
	body, err := json.Marshal(map[string]string{
		"bootstrapToken": bootstrapToken,
		"enrollmentId":   c.store.EnrollmentID(),
		"nodeName":       c.nodeName,
		"publicKey":      c.store.PublicKey(),
		"challengeId":    challenge.ChallengeID,
		"signature":      signature,
	})
	if err != nil {
		return Identity{}, err
	}
	raw, err := c.post(ctx, "/judge/nodes/enroll", body)
	if err != nil {
		return Identity{}, err
	}
	var identity Identity
	if err := decodeData(raw, &identity); err != nil {
		return Identity{}, fmt.Errorf("malformed enroll response: %w", err)
	}
	if identity.NodeID == "" || identity.KeyID == "" {
		return Identity{}, errors.New("enroll response missing nodeId/keyId")
	}
	return identity, nil
}

func (c *Client) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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
		return nil, errors.New("enrollment response body too large")
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, &auth.DeniedError{StatusCode: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("enrollment endpoint returned http %d", resp.StatusCode)
	}
	return raw, nil
}

func decodeData(raw []byte, dst any) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	switch env.Code {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return &auth.DeniedError{Code: env.Code}
	default:
		// 服务端临时故障（Redis/DB/5xx）可重试；只有明确拒绝才永久停止。
		return fmt.Errorf("enrollment endpoint returned result code %d", env.Code)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return errors.New("empty response data")
	}
	return json.Unmarshal(env.Data, dst)
}
