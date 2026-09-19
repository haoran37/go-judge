// Package identity 管理判题节点本地长期身份：真实随机 Ed25519 密钥、enrollmentId、
// 服务端分配的 nodeId/keyId、轮换中的 pending key 与 grace 状态。
// 私钥只落在本地 0700 目录/0600 文件（Windows 用 current-user-only ACL），
// 原子写入并 fsync，绝不进入日志或 WebUI 响应。Token 不是身份。
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/protocol"
	"github.com/criyle/go-judge/internal/hnieoj/securefile"
)

// Key 状态。
const (
	KeyActive  = "ACTIVE"
	KeyGrace   = "GRACE"
	KeyPending = "PENDING"
	KeyRevoked = "REVOKED"
)

// stateVersion 是本地身份文件格式版本。
const stateVersion = 1

// KeyRecord 是一条本地密钥事实。PrivateKey 为标准 Base64 的 32 字节 seed，本地专用。
type KeyRecord struct {
	KeyID      string `json:"keyId,omitempty"`
	PublicKey  string `json:"publicKey"`
	PrivateKey string `json:"privateKey"`
	Status     string `json:"status"`
	CreatedAt  string `json:"createdAt,omitempty"`
	// GraceUntil 是 GRACE 状态的硬截止（Unix 毫秒），到此之后旧 key 不再可用。
	GraceUntil int64 `json:"graceUntil,omitempty"`
}

// PendingRotation 是轮换两阶段状态，持久化后即使崩溃也能凭它恢复/确认。
type PendingRotation struct {
	RotationID   string `json:"rotationId"`
	NewKeyID     string `json:"newKeyId,omitempty"`
	NewPublicKey string `json:"newPublicKey"`
	NewKey       string `json:"newKey"`
	ConfirmNonce string `json:"confirmNonce,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
	// Confirmed 表示已经收到服务端确认，崩溃重启后只需切换本地状态。
	Confirmed  bool   `json:"confirmed,omitempty"`
	GraceUntil int64  `json:"graceUntil,omitempty"`
	CreatedAt  string `json:"createdAt,omitempty"`
}

// fileState 是落盘格式。
type fileState struct {
	Version         int              `json:"version"`
	EnrollmentID    string           `json:"enrollmentId"`
	NodeName        string           `json:"nodeName"`
	NodeType        string           `json:"nodeType"`
	NodeID          string           `json:"nodeId,omitempty"`
	CurrentKeyID    string           `json:"currentKeyId,omitempty"`
	Keys            []KeyRecord      `json:"keys"`
	PendingRotation *PendingRotation `json:"pendingRotation,omitempty"`
	CreatedAt       string           `json:"createdAt"`
	UpdatedAt       string           `json:"updatedAt"`
}

// Public 是可以安全展示给 WebUI 的身份元数据，不含任何私钥。
type Public struct {
	EnrollmentID    string `json:"enrollmentId"`
	NodeID          string `json:"nodeId"`
	KeyID           string `json:"keyId"`
	NodeName        string `json:"nodeName"`
	NodeType        string `json:"nodeType"`
	PublicKey       string `json:"publicKey"`
	KeyStatus       string `json:"keyStatus"`
	RotationID      string `json:"rotationId,omitempty"`
	RotationStatus  string `json:"rotationStatus,omitempty"`
	RotationKeyID   string `json:"rotationKeyId,omitempty"`
	RotationExpires int64  `json:"rotationExpiresAt,omitempty"`
	GraceKeyID      string `json:"graceKeyId,omitempty"`
	GraceUntil      int64  `json:"graceUntil,omitempty"`
}

// Store 持有身份状态并串行化读写。
type Store struct {
	path string
	mu   sync.Mutex
	st   fileState
	// now 返回服务端校正后的时间，仅用于 GRACE 授权截止判断；为 nil 时用本地时钟。
	// 与 WSS.ServerNow 同源，避免本地时钟偏差提前/推迟 grace 失效。
	now func() time.Time
}

// Open 打开或首次创建身份文件。首次调用会在任何入网请求前生成并原子持久化
// 真实随机 Ed25519 keypair 与 enrollmentId；重启只复用，不消耗新 Bootstrap。
func Open(path, nodeName, nodeType string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("identity file path is required")
	}
	s := &Store{path: path, now: time.Now}
	// 已有文件可能是从更宽松权限复制而来：加载前先加固目录/文件，或 fail-closed。
	if err := securefile.HardenExisting(path, 0o600); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		var st fileState
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil, fmt.Errorf("parse identity file: %w", err)
		}
		if st.Version != stateVersion {
			return nil, fmt.Errorf("unsupported identity file version %d", st.Version)
		}
		if err := validateFileState(&st); err != nil {
			return nil, err
		}
		if nodeName != "" && st.NodeName == "" {
			st.NodeName = nodeName
		}
		if nodeType != "" && st.NodeType == "" {
			st.NodeType = nodeType
		}
		s.st = st
		return s, nil
	case errors.Is(err, os.ErrNotExist):
		st, err := newFileState(nodeName, nodeType)
		if err != nil {
			return nil, err
		}
		if err := s.commitLocked(*st); err != nil {
			return nil, fmt.Errorf("persist initial identity: %w", err)
		}
		return s, nil
	default:
		return nil, err
	}
}

func newFileState(nodeName, nodeType string) (*fileState, error) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	seed := privateKey.Seed()
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return &fileState{
		Version:      stateVersion,
		EnrollmentID: randomID(16),
		NodeName:     nodeName,
		NodeType:     nodeType,
		Keys: []KeyRecord{{
			PublicKey:  protocol.EncodePublicKey(publicKey),
			PrivateKey: base64.StdEncoding.EncodeToString(seed),
			Status:     KeyActive,
			CreatedAt:  now,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func validateFileState(st *fileState) error {
	if strings.TrimSpace(st.EnrollmentID) == "" {
		return errors.New("identity file missing enrollmentId")
	}
	if len(st.Keys) == 0 {
		return errors.New("identity file has no keys")
	}
	for i := range st.Keys {
		seed, err := decodeSeed(st.Keys[i].PrivateKey)
		if err != nil {
			return fmt.Errorf("identity key %d: %w", i, err)
		}
		public, err := protocol.DecodePublicKey(st.Keys[i].PublicKey)
		if err != nil {
			return fmt.Errorf("identity key %d: %w", i, err)
		}
		// 拒绝损坏/公私混配：私钥派生的公钥必须与持久化公钥一致。
		derived := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
		if !derived.Equal(public) {
			return fmt.Errorf("identity key %d: private/public key mismatch", i)
		}
		switch st.Keys[i].Status {
		case KeyActive, KeyGrace, KeyPending, KeyRevoked:
		default:
			return fmt.Errorf("identity key %d: unknown status %q", i, st.Keys[i].Status)
		}
	}
	return nil
}

// Public 返回可安全展示的元数据快照。
func (s *Store) Public() Public {
	s.mu.Lock()
	defer s.mu.Unlock()
	pub := Public{
		EnrollmentID: s.st.EnrollmentID,
		NodeID:       s.st.NodeID,
		KeyID:        s.st.CurrentKeyID,
		NodeName:     s.st.NodeName,
		NodeType:     s.st.NodeType,
	}
	if key, ok := s.currentKeyLocked(); ok {
		pub.PublicKey = key.PublicKey
		pub.KeyStatus = key.Status
	}
	for _, key := range s.st.Keys {
		if key.Status == KeyGrace {
			pub.GraceKeyID = key.KeyID
			pub.GraceUntil = key.GraceUntil
		}
	}
	if s.st.PendingRotation != nil {
		pub.RotationID = s.st.PendingRotation.RotationID
		pub.RotationKeyID = s.st.PendingRotation.NewKeyID
		pub.RotationExpires = s.st.PendingRotation.ExpiresAt
		if s.st.PendingRotation.Confirmed {
			pub.RotationStatus = "CONFIRMED_PENDING_SWITCH"
		} else {
			pub.RotationStatus = "PENDING"
		}
	}
	return pub
}

// EnrollmentID 返回持久化的 enrollmentId，供重复注册恢复同一 node。
func (s *Store) EnrollmentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.EnrollmentID
}

// NodeName 返回持久化的注册节点名；非空时注册请求必须复用它，不能随配置改变绑定内容。
func (s *Store) NodeName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.NodeName
}

// NodeID 返回服务端分配并持久化的 nodeId（注册前为空）。
func (s *Store) NodeID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.NodeID
}

// PublicKey 返回当前身份公钥（用于登记/认证）。
func (s *Store) PublicKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key, ok := s.currentKeyLocked(); ok {
		return key.PublicKey
	}
	return ""
}

// CurrentKeyID 返回当前 keyId；注册前为空。
func (s *Store) CurrentKeyID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.CurrentKeyID
}

// CurrentKeyCreatedAt 返回当前 key 的本地创建时间，用于按持久时间计算轮换 due。
func (s *Store) CurrentKeyCreatedAt() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.currentKeyLocked()
	if !ok || key.CreatedAt == "" {
		return time.Time{}, false
	}
	created, err := time.Parse(time.RFC3339, key.CreatedAt)
	if err != nil {
		return time.Time{}, false
	}
	return created, true
}

// CurrentPrivateKey 返回当前激活私钥；始终是真实随机 Ed25519 seed。
func (s *Store) CurrentPrivateKey() (ed25519.PrivateKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.currentKeyLocked()
	if !ok {
		return nil, errors.New("identity has no active key")
	}
	seed, err := decodeSeed(key.PrivateKey)
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// SetNow 注入服务端校正后的时钟；仅影响 GRACE 等授权截止判断，不改变本地持久化时间戳。
func (s *Store) SetNow(now func() time.Time) {
	if now == nil {
		return
	}
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

// nowLocked 返回授权判断使用的时间；调用方必须持有 s.mu。
func (s *Store) nowLocked() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// PrivateKeyByID 返回指定 keyId 的私钥，仅限 ACTIVE 与仍处于 grace 窗口内的 GRACE。
// PENDING 私钥只允许通过 pending 恢复专用路径使用，绝不作为普通业务签名 key。
func (s *Store) PrivateKeyByID(keyID string) (ed25519.PrivateKey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowLocked().UnixMilli()
	for i := range s.st.Keys {
		key := s.st.Keys[i]
		if key.KeyID != keyID {
			continue
		}
		switch key.Status {
		case KeyActive:
		case KeyGrace:
			if key.GraceUntil > 0 && now >= key.GraceUntil {
				return nil, false
			}
		default:
			return nil, false
		}
		seed, err := decodeSeed(key.PrivateKey)
		if err != nil {
			return nil, false
		}
		return ed25519.NewKeyFromSeed(seed), true
	}
	return nil, false
}

// HasSigningKey 返回本地是否持有可签名 keyId（ACTIVE/GRACE 或轮换中的 pending 新 key）。
func (s *Store) HasSigningKey(keyID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.PendingRotation != nil && s.st.PendingRotation.NewKeyID == keyID {
		return true
	}
	now := s.nowLocked().UnixMilli()
	for i := range s.st.Keys {
		key := s.st.Keys[i]
		if key.KeyID != keyID {
			continue
		}
		switch key.Status {
		case KeyActive:
			return true
		case KeyGrace:
			return !(key.GraceUntil > 0 && now >= key.GraceUntil)
		}
	}
	return false
}

// PendingPrivateKey 返回轮换中 pending 新 key 的 keyId 与私钥（仅用于受限的轮换恢复路径）。
func (s *Store) PendingPrivateKey() (string, ed25519.PrivateKey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.st.PendingRotation
	if pending == nil || pending.NewKeyID == "" {
		return "", nil, false
	}
	seed, err := decodeSeed(pending.NewKey)
	if err != nil {
		return "", nil, false
	}
	return pending.NewKeyID, ed25519.NewKeyFromSeed(seed), true
}

// Sign 用当前激活私钥对合同字段签名。
func (s *Store) Sign(fields ...string) (string, error) {
	key, err := s.CurrentPrivateKey()
	if err != nil {
		return "", err
	}
	return protocol.Sign(key, fields...), nil
}

// SignWith 用指定 keyId 的私钥签名，供轮换确认/grace 期使用。
func (s *Store) SignWith(keyID string, fields ...string) (string, error) {
	key, ok := s.PrivateKeyByID(keyID)
	if !ok {
		return "", fmt.Errorf("private key %q not available", keyID)
	}
	return protocol.Sign(key, fields...), nil
}

// SignWithCredential 用 token 绑定的 keyId 签名。除 ACTIVE/GRACE 外，还允许服务端已认证
// 但本地仍处于 pending 的轮换新 key：confirm 回包丢失后服务端可能已将其置为 ACTIVE，
// 而本地尚未切换；此时 token 绑定的就是这个 pending key。它是唯一允许用未本地激活 key
// 签业务请求的受限路径，绝不据此激活本地 key，也不回退到其他无关 key。
func (s *Store) SignWithCredential(keyID string, fields ...string) (string, error) {
	if key, ok := s.PrivateKeyByID(keyID); ok {
		return protocol.Sign(key, fields...), nil
	}
	s.mu.Lock()
	pending := s.st.PendingRotation
	s.mu.Unlock()
	if pending != nil && pending.NewKeyID != "" && pending.NewKeyID == keyID {
		return s.SignPending(fields...)
	}
	return "", fmt.Errorf("private key %q not available", keyID)
}

// SignPending 用 pending rotation 的新私钥签名（prepare 的 newKeyProof）。
func (s *Store) SignPending(fields ...string) (string, error) {
	s.mu.Lock()
	pending := s.st.PendingRotation
	s.mu.Unlock()
	if pending == nil {
		return "", errors.New("no pending rotation")
	}
	seed, err := decodeSeed(pending.NewKey)
	if err != nil {
		return "", err
	}
	return protocol.Sign(ed25519.NewKeyFromSeed(seed), fields...), nil
}

// ApplyEnrollment 记录服务端分配的身份并原子持久化。
func (s *Store) ApplyEnrollment(nodeID, keyID string) error {
	nodeID = strings.TrimSpace(nodeID)
	keyID = strings.TrimSpace(keyID)
	if nodeID == "" || keyID == "" {
		return errors.New("enrollment requires nodeId and keyId")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.st)
	next.NodeID = nodeID
	next.CurrentKeyID = keyID
	if key, ok := currentKeyOf(next); ok {
		next.Keys[keyIndex(next.Keys, key.PublicKey)].KeyID = keyID
	} else if len(next.Keys) > 0 {
		next.Keys[0].KeyID = keyID
	}
	next.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return s.commitLocked(next)
}

// BeginRotation 在调用服务端 prepare 之前先本地生成并持久化新 keypair 与 rotationId，
// 保证响应丢失或崩溃后仍能用同一 rotationId/公钥重试，而不会丢失唯一可用密钥。
// 采用 copy→persist→publish：任何持久化失败都不会在内存中暴露未落盘的 pending key。
func (s *Store) BeginRotation(rotationID string) (PendingRotation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.PendingRotation != nil {
		return *s.st.PendingRotation, nil
	}
	if strings.TrimSpace(rotationID) == "" {
		rotationID = randomID(16)
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return PendingRotation{}, err
	}
	newPublic := protocol.EncodePublicKey(privateKey.Public().(ed25519.PublicKey))
	pending := PendingRotation{
		RotationID:   rotationID,
		NewPublicKey: newPublic,
		NewKey:       base64.StdEncoding.EncodeToString(privateKey.Seed()),
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	now := time.Now().UTC().Format(time.RFC3339)
	next := cloneState(s.st)
	next.Keys = append(next.Keys, KeyRecord{
		PublicKey:  newPublic,
		PrivateKey: pending.NewKey,
		Status:     KeyPending,
		CreatedAt:  now,
	})
	next.PendingRotation = &pending
	next.UpdatedAt = now
	if err := s.commitLocked(next); err != nil {
		return PendingRotation{}, err
	}
	return pending, nil
}

// RecordRotationPrepared 记录 prepare 响应中的 keyId/confirmNonce/expiresAt。
// 同一 rotationId 且公钥一致时幂等更新。
func (s *Store) RecordRotationPrepared(rotationID, keyID, confirmNonce string, expiresAt int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.st)
	pending := next.PendingRotation
	if pending == nil || pending.RotationID != rotationID {
		return fmt.Errorf("pending rotation %q not found", rotationID)
	}
	if keyID != "" {
		pending.NewKeyID = keyID
		pending.ConfirmNonce = confirmNonce
		pending.ExpiresAt = expiresAt
		if idx := keyIndexByPublic(next.Keys, pending.NewPublicKey); idx >= 0 {
			next.Keys[idx].KeyID = keyID
		}
	}
	next.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return s.commitLocked(next)
}

// MarkRotationConfirmed 记录已收到服务端确认，但尚未切换本地默认 key。
func (s *Store) MarkRotationConfirmed(graceUntil int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.PendingRotation == nil {
		return errors.New("no pending rotation to confirm")
	}
	next := cloneState(s.st)
	next.PendingRotation.Confirmed = true
	next.PendingRotation.GraceUntil = graceUntil
	next.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return s.commitLocked(next)
}

// ActivateRotatedKey 把新 key 切为 ACTIVE、旧 key 转 GRACE（带硬截止），并清除 pending。
// 只有已确认的轮换才允许切换，绝不删除唯一可用密钥。
func (s *Store) ActivateRotatedKey() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.PendingRotation == nil || !s.st.PendingRotation.Confirmed {
		return errors.New("rotation is not confirmed")
	}
	next := cloneState(s.st)
	pending := next.PendingRotation
	oldID := next.CurrentKeyID
	oldPublic := ""
	if key, ok := currentKeyOf(next); ok {
		oldPublic = key.PublicKey
	}
	newIdx := keyIndexByPublic(next.Keys, pending.NewPublicKey)
	if newIdx < 0 {
		return errors.New("pending rotation key missing locally")
	}
	if oldIdx := keyIndexByPublic(next.Keys, oldPublic); oldIdx >= 0 {
		next.Keys[oldIdx].Status = KeyGrace
		next.Keys[oldIdx].GraceUntil = pending.GraceUntil
		if next.Keys[oldIdx].KeyID == "" {
			next.Keys[oldIdx].KeyID = oldID
		}
	}
	next.Keys[newIdx].Status = KeyActive
	next.CurrentKeyID = next.Keys[newIdx].KeyID
	next.PendingRotation = nil
	next.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return s.commitLocked(next)
}

// ExpireGrace 清理已过 grace 的旧 key，避免永远保留 old key。返回是否发生变更。
func (s *Store) ExpireGrace(now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.st)
	kept := next.Keys[:0]
	changed := false
	for _, key := range next.Keys {
		if key.Status == KeyGrace && key.GraceUntil > 0 && now.UnixMilli() >= key.GraceUntil {
			changed = true
			continue
		}
		kept = append(kept, key)
	}
	if !changed {
		return false, nil
	}
	if len(kept) == 0 {
		return false, errors.New("refusing to remove all identity keys")
	}
	next.Keys = kept
	next.UpdatedAt = now.UTC().Format(time.RFC3339)
	if err := s.commitLocked(next); err != nil {
		return false, err
	}
	return true, nil
}

// ExpirePendingRotation 已不再凭本地 prepare 截止时间删除 pending。
// confirm 响应丢失时服务端可能已经激活新 key，本地时间到点就删除会丢掉唯一可用的新私钥，
// 使节点再也无法用服务端认可的新 key 认证。服务端才是轮换状态的权威：只有权威返回
// EXPIRED（或明确未生效）时才允许调用 DiscardPendingRotation。该方法保留为显式 no-op，
// 让“本地截止时间不构成删除依据”成为可测试的契约，pending 会一直保留到被权威判定或重启恢复。
func (s *Store) ExpirePendingRotation(now time.Time) (bool, error) {
	_ = now
	return false, nil
}

// DiscardPendingRotation 丢弃服务端已权威判定 EXPIRED/未生效的 pending rotation。
// 绝不删除当前 ACTIVE key；仅移除这一轮生成、尚未激活的 pending 新 key。
func (s *Store) DiscardPendingRotation(now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.st.PendingRotation
	if pending == nil || pending.Confirmed {
		return false, nil
	}
	next := cloneState(s.st)
	if idx := keyIndexByPublic(next.Keys, pending.NewPublicKey); idx >= 0 {
		if next.CurrentKeyID != "" && next.Keys[idx].KeyID == next.CurrentKeyID {
			return false, errors.New("refusing to discard the current active identity key")
		}
		next.Keys = append(next.Keys[:idx], next.Keys[idx+1:]...)
	}
	next.PendingRotation = nil
	next.UpdatedAt = now.UTC().Format(time.RFC3339)
	return true, s.commitLocked(next)
}

// Pending 返回 pending rotation 的副本。
func (s *Store) Pending() *PendingRotation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.PendingRotation == nil {
		return nil
	}
	cp := *s.st.PendingRotation
	return &cp
}

func (s *Store) currentKeyLocked() (KeyRecord, bool) {
	return currentKeyOf(s.st)
}

func currentKeyOf(st fileState) (KeyRecord, bool) {
	if st.CurrentKeyID != "" {
		for i := range st.Keys {
			if st.Keys[i].KeyID == st.CurrentKeyID {
				return st.Keys[i], true
			}
		}
	}
	for i := range st.Keys {
		if st.Keys[i].Status == KeyActive {
			return st.Keys[i], true
		}
	}
	if len(st.Keys) > 0 {
		return st.Keys[0], true
	}
	return KeyRecord{}, false
}

// cloneState 深拷贝可变部分，保证先写盘成功再发布到内存。
func cloneState(st fileState) fileState {
	cp := st
	cp.Keys = append([]KeyRecord(nil), st.Keys...)
	if st.PendingRotation != nil {
		pending := *st.PendingRotation
		cp.PendingRotation = &pending
	}
	return cp
}

// commitLocked 先原子持久化 next，成功后替换内存状态；失败时内存保持旧状态。
func (s *Store) commitLocked(next fileState) error {
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := securefile.WriteFileAtomic(s.path, raw, 0o600); err != nil {
		return err
	}
	s.st = next
	return nil
}

func keyIndex(keys []KeyRecord, publicKey string) int {
	return keyIndexByPublic(keys, publicKey)
}

func keyIndexByPublic(keys []KeyRecord, publicKey string) int {
	for i := range keys {
		if keys[i].PublicKey == publicKey {
			return i
		}
	}
	return -1
}

func decodeSeed(encoded string) ([]byte, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode private seed: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("private seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return seed, nil
}

// randomID 生成 URL-safe 随机标识（enrollmentId/rotationId）。
func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// RandomNonce 生成标准 Base64 的 32 字节 nonce（challenge/confirm）。
func RandomNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// Dir 返回身份文件所在目录。
func (s *Store) Dir() string {
	return filepath.Dir(s.path)
}
