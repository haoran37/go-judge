// Package auth 管理逐节点运行凭证：formal 由运维交付，temp 首次用授权码注册，
// 运行期统一用 /judge/nodes/token/renew 稳定续期。凭证以 0600 原子写入文件。
package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
)

// maxResponseBytes 限制后端响应体大小，避免异常大响应拖垮节点。
const maxResponseBytes = 1 << 20

// DeniedError 表示后端明确拒绝访问（HTTP 401/403 或 Result.code 401/403）。
// 这类错误不应重试，应立即停止领取与续期。
type DeniedError struct {
	StatusCode int
	Code       int
	Msg        string
}

func (e *DeniedError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("backend denied request: http %d code %d: %s", e.StatusCode, e.Code, e.Msg)
	}
	return fmt.Sprintf("backend denied request: http %d code %d", e.StatusCode, e.Code)
}

// IsDenied 判断错误是否属于不可重试的后端拒绝。
func IsDenied(err error) bool {
	var denied *DeniedError
	return errors.As(err, &denied)
}

// Credential 是线程安全的逐节点运行凭证。
type Credential struct {
	mu             sync.RWMutex
	NodeID         string
	TokenID        string
	NodeType       string
	HeaderName     string
	HeaderValue    string
	ExpireTime     time.Time
	expireAtMillis int64
	revoked        bool
}

func (c *Credential) Apply(req *http.Request) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.HeaderName != "" && c.HeaderValue != "" {
		req.Header.Set(c.HeaderName, c.HeaderValue)
	}
}

// Expired 依据后端签发 JWT 的 exp（Unix 毫秒）判断，避免依赖节点本地时区。
func (c *Credential) Expired(now time.Time) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.expiredLocked(now)
}

func (c *Credential) expiredLocked(now time.Time) bool {
	if c.expireAtMillis > 0 {
		return now.UnixMilli() >= c.expireAtMillis
	}
	if !c.ExpireTime.IsZero() {
		return !now.Before(c.ExpireTime)
	}
	return false
}

func (c *Credential) ExpiresAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.expireAtMillis > 0 {
		return time.UnixMilli(c.expireAtMillis)
	}
	return c.ExpireTime
}

// ExpireAtMillis 返回用于调度的绝对过期时间（毫秒）；0 表示未知。
func (c *Credential) ExpireAtMillis() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.expireAtMillis
}

func (c *Credential) Revoked() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.revoked
}

func (c *Credential) MarkRevoked() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revoked = true
}

// Snapshot 返回凭证的只读副本，供续期调度使用。
func (c *Credential) Snapshot() Credential {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Credential{
		NodeID:         c.NodeID,
		TokenID:        c.TokenID,
		NodeType:       c.NodeType,
		HeaderName:     c.HeaderName,
		HeaderValue:    c.HeaderValue,
		ExpireTime:     c.ExpireTime,
		expireAtMillis: c.expireAtMillis,
		revoked:        c.revoked,
	}
}

// Replace 用新凭证覆盖当前凭证（续期成功后调用）。
func (c *Credential) Replace(next *Credential) {
	if next == nil {
		return
	}
	next.mu.RLock()
	snapshot := Credential{
		NodeID:         next.NodeID,
		TokenID:        next.TokenID,
		NodeType:       next.NodeType,
		HeaderName:     next.HeaderName,
		HeaderValue:    next.HeaderValue,
		ExpireTime:     next.ExpireTime,
		expireAtMillis: next.expireAtMillis,
	}
	next.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.NodeID = snapshot.NodeID
	c.TokenID = snapshot.TokenID
	c.NodeType = snapshot.NodeType
	c.HeaderName = snapshot.HeaderName
	c.HeaderValue = snapshot.HeaderValue
	c.ExpireTime = snapshot.ExpireTime
	c.expireAtMillis = snapshot.expireAtMillis
	c.revoked = false
}

// storedCredential 是落盘的凭证文件格式，仅保存运行凭证，不含任何全局密钥。
type storedCredential struct {
	NodeID         string `json:"nodeId"`
	TokenID        string `json:"tokenId"`
	NodeType       string `json:"nodeType"`
	TokenType      string `json:"tokenType"`
	Token          string `json:"token"`
	ExpireTime     string `json:"expireTime,omitempty"`
	ExpireAtMillis int64  `json:"expireAtMillis,omitempty"`
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type credentialPayload struct {
	Token      string `json:"token"`
	TokenType  string `json:"tokenType"`
	NodeID     string `json:"nodeId"`
	TokenID    string `json:"tokenId"`
	ExpireTime string `json:"expireTime"`
}

// Manager 负责加载、持有并续期节点凭证。
type Manager struct {
	cfg      config.Config
	client   *http.Client
	logger   logging.Logger
	filePath string
	cred     *Credential
}

// Load 按允许的来源加载凭证：
//  1. tokenFile 中已有的运行凭证（支持重启后继续使用最新续期凭证）；
//  2. 内联的逐节点 token；
//  3. temp 节点首次入网的 authCode（仅此一次）。
func Load(ctx context.Context, cfg config.Config, client *http.Client, logger logging.Logger) (*Manager, error) {
	m := &Manager{
		cfg:      cfg,
		client:   client,
		logger:   logger,
		filePath: strings.TrimSpace(cfg.HnieOJ.Credential.TokenFile),
	}
	if m.filePath != "" {
		stored, err := readCredentialFile(m.filePath)
		switch {
		case err == nil && stored.Token != "":
			m.cred = credentialFromStored(stored, cfg.Node.Type)
			m.persistWithWarning()
			return m, nil
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("read credential file: %w", err)
		}
	}

	if token := strings.TrimSpace(cfg.HnieOJ.Credential.Token); token != "" {
		m.cred = credentialFromToken(token, cfg.Node.Type, "", "")
		m.persistWithWarning()
		return m, nil
	}

	if cfg.Node.Type == "temp" && strings.TrimSpace(cfg.HnieOJ.Credential.AuthCode) != "" {
		cred, err := m.exchangeTempToken(ctx)
		if err != nil {
			return nil, err
		}
		m.cred = cred
		if err := m.persist(); err != nil {
			// 首次注册后无法持久化会导致重启需要重新兑换授权码，属于配置错误，直接报错。
			return nil, fmt.Errorf("persist first-enrollment credential: %w", err)
		}
		return m, nil
	}

	return nil, errors.New("no judge node credential available: provide tokenFile, token, or temp authCode")
}

func (m *Manager) Credential() *Credential {
	return m.cred
}

// StartRenewal 启动后台续期协程；所有成功/失败/取消路径都会退出，不会泄漏。
func (m *Manager) StartRenewal(ctx context.Context) {
	go m.renewLoop(ctx)
}

// minRenewInterval 是到期时间未知或剩余有效期无法解析时的兜底间隔，防止紧凑自旋；
// 正常推进式续期仍按 exp-safetyMargin 精确调度。
const minRenewInterval = time.Second

// renewShortTTLDivisor 控制短 TTL 的续期节奏：新的有效期仍落在安全余量内时，
// 至多等待剩余有效期的 1/renewShortTTLDivisor，保证不会睡过新的过期时间。
const renewShortTTLDivisor = 3

func (m *Manager) renewLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		snapshot := m.cred.Snapshot()
		if snapshot.revoked {
			return
		}
		if wait := nextRenewDelay(&snapshot, m.cfg.HnieOJ.Renew.SafetyMargin, time.Now()); wait > 0 {
			if !sleepContext(ctx, wait) {
				return
			}
			continue
		}
		previousExpireAtMillis := snapshot.ExpireAtMillis()
		next, err := m.renew(ctx)
		if err != nil {
			if IsDenied(err) {
				m.cred.MarkRevoked()
				m.logger.Warn("credential renewal denied, stopping node renewal", logging.Error(err))
				return
			}
			m.logger.Warn("credential renewal failed, will retry", logging.Error(err))
			if !sleepContext(ctx, m.cfg.HnieOJ.Renew.RetryBackoff) {
				return
			}
			continue
		}
		m.cred.Replace(next)
		if err := m.persist(); err != nil {
			m.logger.Warn("credential persist failed", logging.Error(err))
		}
		m.logger.Info("credential renewed",
			logging.String("nodeId", next.NodeID), logging.String("tokenId", next.TokenID))
		if !m.rescheduleAfterRenew(ctx, previousExpireAtMillis) {
			return
		}
	}
}

// rescheduleAfterRenew 处理续期成功后的调度：
//   - 后端把有效期推进到安全余量之外时返回 true，由下一轮按 exp 正常调度；
//   - 有效期仍落在安全余量内（短 TTL）时，等待剩余有效期的一小部分后再次续期，
//     保证不会睡过新的过期时间；
//   - temp 节点受授权期上限约束、后端无法再推进有效期时，继续续期没有意义，
//     凭证保留到自然过期后干净退出；
//   - 凭证已实际过期则标记撤销并停止续期。
func (m *Manager) rescheduleAfterRenew(ctx context.Context, previousExpireAtMillis int64) bool {
	updated := m.cred.Snapshot()
	now := time.Now()
	safetyMargin := m.cfg.HnieOJ.Renew.SafetyMargin
	if safetyMargin <= 0 {
		safetyMargin = 30 * time.Second
	}
	if updated.Expired(now) {
		m.logger.Warn("credential expired and backend cannot extend it, stopping renewal",
			logging.String("nodeId", updated.NodeID))
		m.cred.MarkRevoked()
		return false
	}
	if nextRenewDelay(&updated, safetyMargin, now) > 0 {
		return true
	}
	remaining := time.Until(updated.ExpiresAt())
	if remaining <= 0 {
		// 到期时间未知或已到点：按正的兜底间隔重试，避免自旋。
		return sleepContext(ctx, minRenewInterval)
	}
	// temp 授权期上限：后端已无法推进有效期，不再续期，保留凭证直到自然过期。
	// 这里不标记 revoked，因为凭证在过期前仍然有效。
	if updated.NodeType == "temp" && updated.ExpireAtMillis() > 0 && updated.ExpireAtMillis() <= previousExpireAtMillis {
		m.logger.Info("temp credential reached authorization limit, stopping renewal at expiry",
			logging.String("nodeId", updated.NodeID))
		sleepContext(ctx, remaining)
		return false
	}
	// 可续期的短 TTL：等待剩余有效期的一小部分，确保下次续期仍发生在过期之前。
	if delay := remaining / renewShortTTLDivisor; delay > 0 {
		return sleepContext(ctx, delay)
	}
	return sleepContext(ctx, minRenewInterval)
}

// nextRenewDelay 只依据 JWT exp 决定续期时机，服务端 LocalDateTime 字符串不作为调度依据。
func nextRenewDelay(cred *Credential, safetyMargin time.Duration, now time.Time) time.Duration {
	if safetyMargin <= 0 {
		safetyMargin = 30 * time.Second
	}
	expireAtMillis := cred.ExpireAtMillis()
	if expireAtMillis <= 0 {
		// exp 未知时按安全余量轮询，避免无界紧凑循环。
		return safetyMargin
	}
	renewAt := time.UnixMilli(expireAtMillis).Add(-safetyMargin)
	if renewAt.After(now) {
		return renewAt.Sub(now)
	}
	return 0
}

func (m *Manager) exchangeTempToken(ctx context.Context) (*Credential, error) {
	body, err := json.Marshal(struct {
		AuthCode string `json:"authCode"`
		NodeName string `json:"nodeName"`
	}{
		AuthCode: strings.TrimSpace(m.cfg.HnieOJ.Credential.AuthCode),
		NodeName: m.cfg.Node.Name,
	})
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(m.cfg.HnieOJ.BaseURL, "/") + "/api/judge/temp-token"
	payload, err := m.doJSON(ctx, http.MethodPost, endpoint, body, nil)
	if err != nil {
		return nil, fmt.Errorf("temp token exchange: %w", err)
	}
	return credentialFromPayload(payload, m.cfg.Node.Type, "", ""), nil
}

func (m *Manager) renew(ctx context.Context) (*Credential, error) {
	snapshot := m.cred.Snapshot()
	body := []byte("{}")
	endpoint := strings.TrimRight(m.cfg.HnieOJ.BaseURL, "/") + "/judge/nodes/token/renew"
	payload, err := m.doJSON(ctx, http.MethodPost, endpoint, body, &snapshot)
	if err != nil {
		return nil, fmt.Errorf("token renew: %w", err)
	}
	// 稳定续期必须保持 nodeId/tokenId；身份漂移视为错误，禁止静默接受。
	if snapshot.NodeID != "" && payload.NodeID != "" && payload.NodeID != snapshot.NodeID {
		return nil, fmt.Errorf("token renew changed nodeId: %s -> %s", snapshot.NodeID, payload.NodeID)
	}
	if snapshot.TokenID != "" && payload.TokenID != "" && payload.TokenID != snapshot.TokenID {
		return nil, fmt.Errorf("token renew changed tokenId: %s -> %s", snapshot.TokenID, payload.TokenID)
	}
	return credentialFromPayload(payload, m.cfg.Node.Type, snapshot.NodeID, snapshot.TokenID), nil
}

// doJSON 发送请求并校验 HTTP 状态与 Result.code。apply 非空时附带 Bearer 头。
func (m *Manager) doJSON(ctx context.Context, method, endpoint string, body []byte, apply *Credential) (credentialPayload, error) {
	var payload credentialPayload
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return payload, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apply != nil {
		apply.Apply(req)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return payload, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return payload, err
	}
	if int64(len(raw)) > maxResponseBytes {
		return payload, errors.New("backend response body too large")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return payload, &DeniedError{StatusCode: resp.StatusCode, Msg: truncateMsg(raw)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return payload, fmt.Errorf("backend returned http %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return payload, fmt.Errorf("malformed backend response: %w", err)
	}
	if env.Code == http.StatusUnauthorized || env.Code == http.StatusForbidden {
		return payload, &DeniedError{StatusCode: resp.StatusCode, Code: env.Code, Msg: env.Msg}
	}
	if env.Code != http.StatusOK {
		return payload, fmt.Errorf("backend result code %d: %s", env.Code, env.Msg)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return payload, errors.New("backend credential response is empty")
	}
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		return payload, fmt.Errorf("malformed credential payload: %w", err)
	}
	if strings.TrimSpace(payload.Token) == "" {
		return payload, errors.New("backend credential token is empty")
	}
	return payload, nil
}

func (m *Manager) persist() error {
	if m.filePath == "" {
		return nil
	}
	return writeCredentialFile(m.filePath, storedCredentialFromCredential(m.cred))
}

// persistWithWarning 在凭证已可用时，持久化失败只告警不阻塞启动。
func (m *Manager) persistWithWarning() {
	if err := m.persist(); err != nil {
		m.logger.Warn("credential persist failed", logging.Error(err))
	}
}

func storedCredentialFromCredential(cred *Credential) storedCredential {
	snapshot := cred.Snapshot()
	tokenType, token := splitHeaderValue(snapshot.HeaderName, snapshot.HeaderValue)
	expireTime := ""
	if !snapshot.ExpireTime.IsZero() {
		expireTime = snapshot.ExpireTime.Format("2006-01-02T15:04:05")
	}
	return storedCredential{
		NodeID:         snapshot.NodeID,
		TokenID:        snapshot.TokenID,
		NodeType:       snapshot.NodeType,
		TokenType:      tokenType,
		Token:          token,
		ExpireTime:     expireTime,
		ExpireAtMillis: snapshot.ExpireAtMillis(),
	}
}

func readCredentialFile(path string) (storedCredential, error) {
	var stored storedCredential
	b, err := os.ReadFile(path)
	if err != nil {
		return stored, err
	}
	if err := json.Unmarshal(b, &stored); err != nil {
		return stored, err
	}
	return stored, nil
}

// writeCredentialFile 以 0600 权限原子替换凭证文件。
func writeCredentialFile(path string, stored storedCredential) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credential-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(stored); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func credentialFromStored(stored storedCredential, fallbackNodeType string) *Credential {
	nodeType := stored.NodeType
	if nodeType == "" {
		nodeType = fallbackNodeType
	}
	return newCredential(stored.NodeID, stored.TokenID, nodeType, stored.TokenType, stored.Token,
		stored.ExpireTime, stored.ExpireAtMillis)
}

func credentialFromToken(token, nodeType, nodeID, tokenID string) *Credential {
	return newCredential(nodeID, tokenID, nodeType, "Bearer", token, "", 0)
}

func credentialFromPayload(payload credentialPayload, fallbackNodeType, fallbackNodeID, fallbackTokenID string) *Credential {
	nodeID := payload.NodeID
	if nodeID == "" {
		nodeID = fallbackNodeID
	}
	tokenID := payload.TokenID
	if tokenID == "" {
		tokenID = fallbackTokenID
	}
	return newCredential(nodeID, tokenID, fallbackNodeType, payload.TokenType, payload.Token, payload.ExpireTime, 0)
}

func newCredential(nodeID, tokenID, nodeType, tokenType, token, expireTime string, expireAtMillis int64) *Credential {
	if tokenType == "" {
		tokenType = "Bearer"
	}
	if expireAtMillis <= 0 {
		expireAtMillis = jwtExpiryMillis(token)
	}
	parsedExpireTime, _ := parseExpireTime(expireTime)
	return &Credential{
		NodeID:         strings.TrimSpace(nodeID),
		TokenID:        strings.TrimSpace(tokenID),
		NodeType:       strings.TrimSpace(nodeType),
		HeaderName:     "Authorization",
		HeaderValue:    tokenType + " " + token,
		ExpireTime:     parsedExpireTime,
		expireAtMillis: expireAtMillis,
	}
}

// splitHeaderValue 从 "Bearer xxx" 还原 tokenType 与 token。
func splitHeaderValue(headerName, headerValue string) (string, string) {
	if headerName != "Authorization" {
		return "", headerValue
	}
	parts := strings.SplitN(strings.TrimSpace(headerValue), " ", 2)
	if len(parts) != 2 {
		return "Bearer", headerValue
	}
	return parts[0], parts[1]
}

// jwtExpiryMillis 只解码 JWT payload 的 exp 供本地调度使用。
// 服务端验签仍是唯一权威；这里不校验签名，也绝不用它做授权判断。
func jwtExpiryMillis(token string) int64 {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(parts[1]); err != nil {
			return 0
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return 0
	}
	return normalizeEpoch(claims["exp"])
}

func normalizeEpoch(value any) int64 {
	var epoch int64
	switch v := value.(type) {
	case float64:
		epoch = int64(v)
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return 0
		}
		epoch = parsed
	case string:
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0
		}
		epoch = parsed
	default:
		return 0
	}
	if epoch <= 0 {
		return 0
	}
	// 后端 JWT 使用 Unix 毫秒；兼容以秒为单位的签发实现。
	if epoch < 1_000_000_000_000 {
		return epoch * 1000
	}
	return epoch
}

func parseExpireTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02T15:04:05", s, time.Local)
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func truncateMsg(raw []byte) string {
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 256 {
		return msg[:256]
	}
	return msg
}
