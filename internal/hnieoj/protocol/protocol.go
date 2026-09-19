// Package protocol 实现 HnieOJ Secure Node Protocol v1 的冻结合同：
// 规范化签名字节、域常量、WSS 信封与消息类型、边界常量。
// 该包的编码必须与 protocol-v1.md / protocol-vectors.json 完全一致。
package protocol

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Version 是当前 WSS 协议版本，未知版本一律拒绝。
const Version = 1

// 签名域常量。每个用途一个独立域，禁止复用。
const (
	DomainEnroll        = "HNIEOJ-ENROLL-V1"
	DomainAuth          = "HNIEOJ-AUTH-V1"
	DomainHTTP          = "HNIEOJ-HTTP-V1"
	DomainRotatePrepare = "HNIEOJ-ROTATE-PREPARE-V1"
	DomainRotateConfirm = "HNIEOJ-ROTATE-CONFIRM-V1"
)

// WSS 消息类型。
const (
	TypeAuthChallenge = "AUTH_CHALLENGE"
	TypeAuthResponse  = "AUTH_RESPONSE"
	TypeAuthOK        = "AUTH_OK"
	TypeAuthRefresh   = "AUTH_REFRESH"
	TypeResumeTasks   = "RESUME_TASKS"
	TypeResumeResult  = "RESUME_RESULT"
	TypeReady         = "READY"
	TypeTaskAssign    = "TASK_ASSIGN"
	TypeTaskAck       = "TASK_ACK"
	TypeTaskRunning   = "TASK_RUNNING"
	TypeTaskEventAck  = "TASK_EVENT_ACK"
	TypeTaskResult    = "TASK_RESULT"
	TypeTaskResultAck = "TASK_RESULT_ACK"
	TypeLeaseRenew    = "LEASE_RENEW"
	TypeLeaseRenewed  = "LEASE_RENEWED"
	TypeTaskCancel    = "TASK_CANCEL"
	TypeHeartbeat     = "HEARTBEAT"
	TypeHeartbeatAck  = "HEARTBEAT_ACK"
	TypeNodeDrain     = "NODE_DRAIN"
	TypeNodeState     = "NODE_STATE"
	TypePing          = "PING"
	TypePong          = "PONG"
	TypeError         = "ERROR"
)

// 帧与时间边界（可配置项都有正的上界）。
const (
	// DefaultControlFrameBytes 是控制帧（认证/心跳/租约等）默认上限 64KiB。
	DefaultControlFrameBytes = 64 * 1024
	// DefaultTaskFrameBytes 是任务/结果帧默认上限 4MiB。
	DefaultTaskFrameBytes = 4 * 1024 * 1024
	// MaxFrameBytes 是允许配置的帧上限硬顶，避免无界内存。
	MaxFrameBytes = 64 * 1024 * 1024
	// AuthDeadline 是连接建立后完成认证的时限。
	AuthDeadline = 10 * time.Second
	// ChallengeTTL 是服务端挑战默认有效期 30s。
	ChallengeTTL = 30 * time.Second
	// TimestampSkew 是入站时间戳允许的偏移窗口 30s。
	TimestampSkew = 30 * time.Second
)

// Envelope 是所有 WSS 消息的统一信封。
type Envelope struct {
	Version   int             `json:"version"`
	Type      string          `json:"type"`
	RequestID string          `json:"requestId"`
	Timestamp int64           `json:"timestamp"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Canonical 按合同的规范化编码拼接字段：每个字段 UTF-8 字节前加 4 字节大端长度。
func Canonical(fields ...string) []byte {
	size := 0
	for _, field := range fields {
		size += 4 + len(field)
	}
	out := make([]byte, 0, size)
	var length [4]byte
	for _, field := range fields {
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		out = append(out, length[:]...)
		out = append(out, field...)
	}
	return out
}

// EnrollFields 返回登记签名覆盖的字段（顺序即合同顺序）。
func EnrollFields(audience, challengeID, nonce, enrollmentID, nodeName, publicKey, bootstrapDigest string) []string {
	return []string{DomainEnroll, audience, challengeID, nonce, enrollmentID, nodeName, publicKey, bootstrapDigest}
}

// AuthFields 返回 WSS 认证签名覆盖的字段。
func AuthFields(audience, challengeID, nonce, nodeID, keyID string) []string {
	return []string{DomainAuth, audience, challengeID, nonce, nodeID, keyID}
}

// HTTPFields 返回签名 HTTPS 请求覆盖的字段。
func HTTPFields(audience, nodeID, keyID, method, pathWithQuery, bodySHA256, timestamp, nonce string) []string {
	return []string{DomainHTTP, audience, nodeID, keyID, strings.ToUpper(method), pathWithQuery, bodySHA256, timestamp, nonce}
}

// RotatePrepareFields 返回轮换 prepare 签名覆盖的字段。
func RotatePrepareFields(audience, nodeID, rotationID, newPublicKey string) []string {
	return []string{DomainRotatePrepare, audience, nodeID, rotationID, newPublicKey}
}

// RotateConfirmFields 返回轮换 confirm 签名覆盖的字段。
func RotateConfirmFields(audience, nodeID, rotationID, keyID, confirmNonce string) []string {
	return []string{DomainRotateConfirm, audience, nodeID, rotationID, keyID, confirmNonce}
}

// Sign 用私钥对规范化字段签名，返回标准 Base64。
func Sign(privateKey ed25519.PrivateKey, fields ...string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, Canonical(fields...)))
}

// Verify 校验标准 Base64 签名。
func Verify(publicKeyBase64 string, signatureBase64 string, fields ...string) error {
	publicKey, err := DecodePublicKey(publicKeyBase64)
	if err != nil {
		return err
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signatureBase64))
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	if len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("signature must be %d bytes, got %d", ed25519.SignatureSize, len(signature))
	}
	if !ed25519.Verify(publicKey, Canonical(fields...), signature) {
		return errors.New("signature verification failed")
	}
	return nil
}

// DecodePublicKey 解析 RFC4648 标准 Base64 的 32 字节 Ed25519 公钥。
func DecodePublicKey(value string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("decode public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// EncodePublicKey 返回标准 Base64 公钥。
func EncodePublicKey(publicKey ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(publicKey)
}

// BodySHA256 返回小写十六进制 SHA256，空 body 也返回空串的 SHA256。
func BodySHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// BootstrapDigest 返回 bootstrapToken 的小写十六进制 SHA256。
func BootstrapDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// KnownType 判断消息类型是否是合同登记的类型。
func KnownType(messageType string) bool {
	switch messageType {
	case TypeAuthChallenge, TypeAuthResponse, TypeAuthOK, TypeAuthRefresh,
		TypeResumeTasks, TypeResumeResult, TypeReady,
		TypeTaskAssign, TypeTaskAck, TypeTaskRunning, TypeTaskEventAck,
		TypeTaskResult, TypeTaskResultAck, TypeLeaseRenew, TypeLeaseRenewed,
		TypeTaskCancel, TypeHeartbeat, TypeHeartbeatAck,
		TypeNodeDrain, TypeNodeState, TypePing, TypePong, TypeError:
		return true
	default:
		return false
	}
}

// ValidateInbound 校验入站信封的版本、类型、requestId 与时间戳偏移。
// nowMillis 为本地时间；serverOffsetMillis 为本地时间到服务端时间的偏移（server-now）。
func ValidateInbound(env *Envelope, nowMillis, serverOffsetMillis, skewMillis int64) error {
	if env == nil {
		return errors.New("nil envelope")
	}
	if env.Version != Version {
		return fmt.Errorf("unsupported protocol version %d", env.Version)
	}
	if !KnownType(env.Type) {
		return fmt.Errorf("unknown message type %q", env.Type)
	}
	if strings.TrimSpace(env.RequestID) == "" {
		return errors.New("missing requestId")
	}
	if env.Timestamp <= 0 {
		return errors.New("missing timestamp")
	}
	if skewMillis <= 0 {
		skewMillis = TimestampSkew.Milliseconds()
	}
	// 以服务端时间为准比较入站时间戳，避免本地时钟漂移误判。
	serverNow := nowMillis + serverOffsetMillis
	delta := serverNow - env.Timestamp
	if delta < 0 {
		delta = -delta
	}
	if delta > skewMillis {
		return fmt.Errorf("timestamp skew %dms exceeds %dms", delta, skewMillis)
	}
	return nil
}
