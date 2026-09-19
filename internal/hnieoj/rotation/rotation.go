// Package rotation 实现合同规定的两阶段密钥轮换：
// 旧 active key 通过签名 HTTPS 发起 prepare，新 key proof 确认；本地先持久化 pending
// 状态，任何响应丢失/崩溃点都能恢复，且绝不提前删除唯一可用密钥。
package rotation

import (
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
	"github.com/criyle/go-judge/internal/hnieoj/httpsign"
	"github.com/criyle/go-judge/internal/hnieoj/identity"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
)

const maxResponseBytes = 1 << 20

type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// PrepareResponse 是 prepare 返回的公开轮换状态。
type PrepareResponse struct {
	RotationID   string `json:"rotationId"`
	KeyID        string `json:"keyId"`
	ConfirmNonce string `json:"confirmNonce"`
	ExpiresAt    int64  `json:"expiresAt"`
}

// QueryResponse 是 GET 返回的公开轮换状态。
type QueryResponse struct {
	RotationID   string `json:"rotationId"`
	KeyID        string `json:"keyId"`
	Status       string `json:"status"`
	NewPublicKey string `json:"newPublicKey"`
	ConfirmNonce string `json:"confirmNonce"`
	ExpiresAt    int64  `json:"expiresAt"`
	GraceUntil   int64  `json:"graceUntil"`
}

// Options 控制轮换节奏与确认。
type Options struct {
	Enabled        bool
	Interval       time.Duration
	Grace          time.Duration
	ConfirmTimeout time.Duration
	// Now 返回服务端校正后的时间，供 grace/pending 截止判断使用；为 nil 时用 time.Now。
	Now func() time.Time
}

// Manager 周期性执行自动轮换。
type Manager struct {
	store     *identity.Store
	signer    *httpsign.Signer
	cred      *auth.Credential
	baseURL   string
	audience  string
	logger    logging.Logger
	opts      Options
	onRotated func()
}

func New(store *identity.Store, signer *httpsign.Signer, cred *auth.Credential, baseURL, audience string, logger logging.Logger, opts Options, onRotated func()) *Manager {
	if opts.Interval <= 0 {
		opts.Interval = 30 * 24 * time.Hour
	}
	if opts.Grace <= 0 {
		opts.Grace = 5 * time.Minute
	}
	if opts.ConfirmTimeout <= 0 {
		opts.ConfirmTimeout = 30 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Manager{
		store:     store,
		signer:    signer,
		cred:      cred,
		baseURL:   strings.TrimRight(baseURL, "/"),
		audience:  audience,
		logger:    logger,
		opts:      opts,
		onRotated: onRotated,
	}
}

// Run 在 ctx 存活期间自动轮换。必须先等到 WSS 认证就绪（拿到访问凭证）才能发起
// 签名 HTTPS；恢复未完成的 pending 轮换用有界短退避重试，并按持久 key 创建时间计算 due。
func (m *Manager) Run(ctx context.Context) {
	if !m.opts.Enabled {
		return
	}
	if !m.waitAuthReady(ctx) {
		return
	}
	// 启动时尝试恢复 pending；失败不阻塞，交给主循环按短退避重试。
	if err := m.Recover(ctx); err != nil {
		m.logger.Warn("rotation recovery failed, will retry", logging.Error(err))
	}

	cleanupInterval := m.opts.Grace
	if cleanupInterval <= 0 || cleanupInterval > time.Minute {
		cleanupInterval = time.Minute
	}
	cleanupTicker := time.NewTicker(cleanupInterval)
	defer cleanupTicker.Stop()
	dueTimer := time.NewTimer(m.nextDue())
	defer dueTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-cleanupTicker.C:
			m.cleanupExpired()
		case <-dueTimer.C:
			m.cleanupExpired()
			if err := m.RotateOnce(ctx); err != nil {
				m.logger.Warn("automatic key rotation failed", logging.Error(err))
			}
			dueTimer.Reset(m.nextDue())
		}
	}
}

// waitAuthReady 等待 WSS 认证产生可用的短期访问凭证，避免在 cred 为空时发签名 HTTPS。
func (m *Manager) waitAuthReady(ctx context.Context) bool {
	for {
		if m.cred != nil && m.cred.AccessTokenValue() != "" {
			return true
		}
		if !sleepContext(ctx, 500*time.Millisecond) {
			return false
		}
	}
}

// recoverWithBackoff 对残留 pending rotation 做有界短退避恢复，直到成功或 ctx 结束。
func (m *Manager) retryBackoff() time.Duration {
	backoff := 2 * time.Second
	if m.opts.ConfirmTimeout > 0 && m.opts.ConfirmTimeout < backoff {
		backoff = m.opts.ConfirmTimeout
	}
	return backoff
}

// nextDue 基于持久 key 创建时间计算下一次轮换时间；有 pending 时按有界短退避重试。
// 时间基准是服务端校正后的 now，避免本地时钟偏差提前/推迟轮换。
func (m *Manager) nextDue() time.Duration {
	if m.store.Pending() != nil {
		return m.retryBackoff()
	}
	created, ok := m.store.CurrentKeyCreatedAt()
	if !ok {
		return m.opts.Interval
	}
	due := created.Add(m.opts.Interval)
	if wait := due.Sub(m.now()); wait > 0 {
		return wait
	}
	return 0
}

// now 返回服务端校正后的时间；未注入时退回本地时钟。
func (m *Manager) now() time.Time {
	if m.opts.Now != nil {
		return m.opts.Now()
	}
	return time.Now()
}

// cleanupExpired 只清理已过 grace 的旧 key，保持唯一可用 key。
// pending 的丢弃必须由服务端权威状态决定，绝不凭本地截止时间删除。
func (m *Manager) cleanupExpired() {
	if changed, err := m.store.ExpireGrace(m.now()); err != nil {
		m.logger.Warn("expire grace failed", logging.Error(err))
	} else if changed {
		m.logger.Info("expired grace key removed")
	}
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

// Recover 处理上一次运行留下的 pending rotation（无论已完成与否）。
func (m *Manager) Recover(ctx context.Context) error {
	pending := m.store.Pending()
	if pending == nil {
		return nil
	}
	m.logger.Info("resuming pending key rotation", logging.String("rotationId", pending.RotationID))
	if pending.Confirmed {
		return m.activate()
	}
	if pending.NewKeyID == "" {
		// prepare 尚未成功记录，重新 prepare（同 rotationId+公钥幂等）。
		return m.prepare(ctx, pending.RotationID, pending.NewPublicKey)
	}
	return m.confirmOrQuery(ctx)
}

// RotateOnce 执行一次完整轮换（若无 pending 则新建）。
func (m *Manager) RotateOnce(ctx context.Context) error {
	if m.store.Pending() != nil {
		return m.Recover(ctx)
	}
	pending, err := m.store.BeginRotation("")
	if err != nil {
		return err
	}
	m.logger.Info("starting key rotation", logging.String("rotationId", pending.RotationID))
	return m.prepare(ctx, pending.RotationID, pending.NewPublicKey)
}

func (m *Manager) prepare(ctx context.Context, rotationID, newPublicKey string) error {
	fields := protocol.RotatePrepareFields(m.audience, m.store.NodeID(), rotationID, newPublicKey)
	proof, err := m.store.SignPending(fields...)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{
		"rotationId":   rotationID,
		"newPublicKey": newPublicKey,
		"newKeyProof":  proof,
	})
	if err != nil {
		return err
	}
	raw, err := m.signed(ctx, http.MethodPost, m.baseURL+"/judge/nodes/keys/rotations", body)
	if err != nil {
		return fmt.Errorf("rotation prepare: %w", err)
	}
	var resp PrepareResponse
	if err := decodeData(raw, &resp); err != nil {
		return fmt.Errorf("malformed rotation prepare response: %w", err)
	}
	if resp.RotationID == "" || resp.KeyID == "" || resp.ConfirmNonce == "" {
		return errors.New("rotation prepare response missing rotationId/keyId/confirmNonce")
	}
	if err := m.store.RecordRotationPrepared(resp.RotationID, resp.KeyID, resp.ConfirmNonce, resp.ExpiresAt); err != nil {
		return err
	}
	return m.confirmOrQuery(ctx)
}

func (m *Manager) confirmOrQuery(ctx context.Context) error {
	return m.confirmOnce(ctx, false)
}

// confirmOnce 发送 confirm；响应丢失时查询服务端状态。recovering 用于避免无限重试循环。
func (m *Manager) confirmOnce(ctx context.Context, recovering bool) error {
	pending := m.store.Pending()
	if pending == nil {
		return nil
	}
	confirmCtx, cancel := context.WithTimeout(ctx, m.opts.ConfirmTimeout)
	defer cancel()
	fields := protocol.RotateConfirmFields(m.audience, m.store.NodeID(), pending.RotationID, pending.NewKeyID, pending.ConfirmNonce)
	signature, err := m.store.SignPending(fields...)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"signature": signature})
	if err != nil {
		return err
	}
	endpoint := m.baseURL + "/judge/nodes/keys/rotations/" + url.PathEscape(pending.RotationID) + "/confirm"
	raw, err := m.signed(confirmCtx, http.MethodPost, endpoint, body)
	if err != nil {
		if recovering {
			return err
		}
		// 响应可能丢失：查询服务端真实状态再决定，绝不盲目重建。
		m.logger.Warn("rotation confirm failed, querying state", logging.Error(err))
		return m.query(ctx, true)
	}
	var resp QueryResponse
	if err := decodeData(raw, &resp); err != nil {
		return fmt.Errorf("malformed rotation confirm response: %w", err)
	}
	if err := validateRotationResponse(pending, &resp); err != nil {
		return err
	}
	if strings.ToUpper(resp.Status) == "EXPIRED" {
		_, expErr := m.store.DiscardPendingRotation(m.now())
		return expErr
	}
	if strings.ToUpper(resp.Status) != "ACTIVE" {
		return fmt.Errorf("rotation confirm returned status %q, expected ACTIVE", resp.Status)
	}
	graceUntil := resp.GraceUntil
	if graceUntil <= 0 {
		return errors.New("rotation confirm missing graceUntil for ACTIVE rotation")
	}
	if err := m.store.MarkRotationConfirmed(graceUntil); err != nil {
		return err
	}
	return m.activate()
}

func (m *Manager) query(ctx context.Context, recovering bool) error {
	pending := m.store.Pending()
	if pending == nil {
		return nil
	}
	endpoint := m.baseURL + "/judge/nodes/keys/rotations/" + url.PathEscape(pending.RotationID)
	raw, err := m.signed(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("rotation query: %w", err)
	}
	var resp QueryResponse
	if err := decodeData(raw, &resp); err != nil {
		return fmt.Errorf("malformed rotation query response: %w", err)
	}
	if err := validateRotationResponse(pending, &resp); err != nil {
		return err
	}
	switch strings.ToUpper(resp.Status) {
	case "ACTIVE":
		graceUntil := resp.GraceUntil
		if graceUntil <= 0 {
			return errors.New("rotation query ACTIVE without graceUntil")
		}
		if err := m.store.MarkRotationConfirmed(graceUntil); err != nil {
			return err
		}
		return m.activate()
	case "EXPIRED":
		_, expErr := m.store.DiscardPendingRotation(m.now())
		return expErr
	default:
		// PENDING：再确认一次；已在恢复路径上则失败退出，避免无限循环。
		if recovering {
			return nil
		}
		return m.confirmOnce(ctx, true)
	}
}

// validateRotationResponse 校验服务端返回的轮换状态确实属于本地这一轮：
// 不允许凭本地猜测激活，也不接受错 rotationId/keyId/公钥或残缺的 ACTIVE 身份响应。
func validateRotationResponse(pending *identity.PendingRotation, resp *QueryResponse) error {
	if pending == nil {
		return errors.New("no pending rotation")
	}
	status := strings.ToUpper(strings.TrimSpace(resp.Status))
	switch status {
	case "ACTIVE", "PENDING", "EXPIRED":
	default:
		return fmt.Errorf("rotation response unknown status %q", resp.Status)
	}
	// rotationId 是这一轮的锚点，任何状态都必须返回且匹配。
	if strings.TrimSpace(resp.RotationID) == "" {
		return errors.New("rotation response missing rotationId")
	}
	if resp.RotationID != pending.RotationID {
		return fmt.Errorf("rotation response rotationId %q does not match pending %q", resp.RotationID, pending.RotationID)
	}
	// 服务端给出的标识必须与本地这一轮一致，绝不据外来 key/公钥激活。
	if resp.KeyID != "" && pending.NewKeyID != "" && resp.KeyID != pending.NewKeyID {
		return fmt.Errorf("rotation response keyId %q does not match pending keyId %q", resp.KeyID, pending.NewKeyID)
	}
	if resp.NewPublicKey != "" && resp.NewPublicKey != pending.NewPublicKey {
		return errors.New("rotation response newPublicKey does not match local pending key")
	}
	if resp.ConfirmNonce != "" && pending.ConfirmNonce != "" && resp.ConfirmNonce != pending.ConfirmNonce {
		return errors.New("rotation response confirmNonce mismatch")
	}
	if resp.ExpiresAt > 0 && pending.ExpiresAt > 0 && resp.ExpiresAt != pending.ExpiresAt {
		return errors.New("rotation response expiresAt mismatch")
	}
	if status == "ACTIVE" {
		// 激活必须基于完整身份：拒绝只有 ACTIVE+graceUntil 的空响应，
		// 也拒绝缺少本地 pending keyId 的响应。
		if strings.TrimSpace(pending.NewKeyID) == "" {
			return errors.New("pending rotation has no keyId to activate")
		}
		if resp.KeyID != pending.NewKeyID {
			return fmt.Errorf("rotation ACTIVE response keyId %q does not match pending %q", resp.KeyID, pending.NewKeyID)
		}
		if resp.NewPublicKey == "" || resp.NewPublicKey != pending.NewPublicKey {
			return errors.New("rotation ACTIVE response missing matching newPublicKey")
		}
	}
	return nil
}

// activate 切换本地默认 key，并触发一次 AUTH_REFRESH 让 token 绑定新 key。
func (m *Manager) activate() error {
	if err := m.store.ActivateRotatedKey(); err != nil {
		return err
	}
	pub := m.store.Public()
	m.logger.Info("key rotation activated",
		logging.String("keyId", pub.KeyID), logging.String("graceKeyId", pub.GraceKeyID))
	if m.onRotated != nil {
		m.onRotated()
	}
	return nil
}

func (m *Manager) signed(ctx context.Context, method, endpoint string, body []byte) ([]byte, error) {
	resp, err := m.signer.Do(ctx, method, endpoint, body, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxResponseBytes {
		return nil, errors.New("rotation response body too large")
	}
	if err := httpsign.ClassifyStatus(resp.StatusCode, raw); err != nil {
		return nil, err
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
		return fmt.Errorf("rotation endpoint returned result code %d", env.Code)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return errors.New("empty rotation response data")
	}
	return json.Unmarshal(env.Data, dst)
}
