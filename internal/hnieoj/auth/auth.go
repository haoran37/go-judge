// Package auth 管理 WSS 短期授权凭证（NODE_ACCESS）与错误分类。
// 身份是本地 Ed25519 私钥；AccessToken 只是短期授权，不是身份，过期后可用有效密钥重新认证。
// 该包不持有任何私钥，私钥只在 identity.Store 中。
package auth

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Credential 是线程安全的短期运行授权快照。
type Credential struct {
	mu sync.RWMutex

	NodeID       string
	KeyID        string
	NodeType     string
	Audience     string
	AccessToken  string
	AccessExpiry int64 // Unix 毫秒
	// SessionEpoch 由服务端在每次成功初始认证时原子递增；换连接可能变化。
	SessionEpoch int64
	// AuthorizationUntil 是身份的业务硬截止（Unix 毫秒），0 表示未知/不限。
	AuthorizationUntil int64
	// KeyGraceUntil 是当前签名 key 的 grace 截止（Unix 毫秒），0 表示无。
	KeyGraceUntil           int64
	MaxConcurrency          int
	SupportedJudgeModes     []string
	HeartbeatIntervalMillis int
	revoked                 bool
}

// Snapshot 返回只读副本。
func (c *Credential) Snapshot() Credential {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Credential{
		NodeID:                  c.NodeID,
		KeyID:                   c.KeyID,
		NodeType:                c.NodeType,
		Audience:                c.Audience,
		AccessToken:             c.AccessToken,
		AccessExpiry:            c.AccessExpiry,
		SessionEpoch:            c.SessionEpoch,
		AuthorizationUntil:      c.AuthorizationUntil,
		KeyGraceUntil:           c.KeyGraceUntil,
		MaxConcurrency:          c.MaxConcurrency,
		SupportedJudgeModes:     append([]string(nil), c.SupportedJudgeModes...),
		HeartbeatIntervalMillis: c.HeartbeatIntervalMillis,
		revoked:                 c.revoked,
	}
}

// Replace 用新快照覆盖当前凭证。
func (c *Credential) Replace(next *Credential) {
	if next == nil {
		return
	}
	snapshot := next.Snapshot()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.NodeID = snapshot.NodeID
	c.KeyID = snapshot.KeyID
	c.NodeType = snapshot.NodeType
	c.Audience = snapshot.Audience
	c.AccessToken = snapshot.AccessToken
	c.AccessExpiry = snapshot.AccessExpiry
	c.SessionEpoch = snapshot.SessionEpoch
	c.AuthorizationUntil = snapshot.AuthorizationUntil
	c.KeyGraceUntil = snapshot.KeyGraceUntil
	c.MaxConcurrency = snapshot.MaxConcurrency
	c.SupportedJudgeModes = snapshot.SupportedJudgeModes
	c.HeartbeatIntervalMillis = snapshot.HeartbeatIntervalMillis
	c.revoked = false
}

// Expired 判断 AccessToken 是否已过期（Unix 毫秒）。
func (c *Credential) Expired(now time.Time) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.expiredLocked(now)
}

func (c *Credential) expiredLocked(now time.Time) bool {
	return c.AccessExpiry > 0 && now.UnixMilli() >= c.AccessExpiry
}

// ExpiresAt 返回 AccessToken 到期时间。
func (c *Credential) ExpiresAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.AccessExpiry <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(c.AccessExpiry)
}

func (c *Credential) AccessTokenValue() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AccessToken
}

func (c *Credential) KeyIDValue() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.KeyID
}

func (c *Credential) NodeIDValue() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.NodeID
}

func (c *Credential) SessionEpochValue() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.SessionEpoch
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

// DeniedError 表示服务端明确拒绝（401/403 或 Result.code 401/403、revoked/expired/epoch stale）。
// 这类错误不应在同一会话/身份上重试，必须停止并等待人工处理，绝不允许自动另注册绕过。
type DeniedError struct {
	StatusCode int
	Code       int
	Msg        string
}

func (e *DeniedError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("server denied request: http %d code %d: %s", e.StatusCode, e.Code, e.Msg)
	}
	return fmt.Sprintf("server denied request: http %d code %d", e.StatusCode, e.Code)
}

// IsDenied 判断错误是否属于不可重试的服务端拒绝。
func IsDenied(err error) bool {
	var denied *DeniedError
	return errors.As(err, &denied) || errors.Is(err, ErrIdentityRejected)
}

// ErrIdentityRejected 表示身份已失效/被撤销，禁止自动重新注册绕过。
var ErrIdentityRejected = errors.New("node identity rejected by server")

// PermanentError 表示服务端返回 retryable=false，不应重建连接重试。
type PermanentError struct {
	Code int
	Msg  string
}

func (e *PermanentError) Error() string {
	return fmt.Sprintf("permanent server error code=%d msg=%s", e.Code, e.Msg)
}

func IsPermanent(err error) bool {
	var permanent *PermanentError
	return errors.As(err, &permanent)
}

// JWTExpiryMillis 只解码 JWT payload 的 exp 供本地调度使用。
// 服务端验签仍是唯一权威；这里不校验签名，也绝不用它做授权判断。
func JWTExpiryMillis(token string) int64 {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return 0
	}
	raw, err := base64URLDecode(parts[1])
	if err != nil {
		return 0
	}
	var claims map[string]any
	if err := jsonUnmarshal(raw, &claims); err != nil {
		return 0
	}
	return normalizeEpoch(claims["exp"])
}
