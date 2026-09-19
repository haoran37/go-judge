// Package httpsign 实现合同规定的签名 HTTPS 请求：
// Bearer NODE_ACCESS + X-Judge-Node-Id/Key-Id/Timestamp/Nonce/Signature，
// 规范化字段包含 method、raw path+query、原始 body 的 SHA256。
// 用于测试数据下载与密钥轮换等受保护 HTTPS 调用；任务通道走 WSS。
package httpsign

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/identity"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
)

// Credential 提供签名 HTTPS 所需的短期授权信息。
// Snapshot 一次性返回一致快照，避免轮换刷新期间拼出不同 key/token 组合。
type Credential interface {
	Snapshot() auth.Credential
}

// Signer 为每个请求附加 Bearer 与 Ed25519 签名头。
type Signer struct {
	identity *identity.Store
	cred     Credential
	audience string
	client   *http.Client
	now      func() time.Time
	nonce    func() (string, error)
}

func New(store *identity.Store, cred Credential, audience string, client *http.Client) *Signer {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// 受保护请求绝不跟随重定向：避免签名头/Authorization 泄露到其他 origin 或降级到 http。
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Signer{
		identity: store,
		cred:     cred,
		audience: audience,
		client:   &clone,
		now:      time.Now,
		nonce:    randomNonce,
	}
}

// Now 允许测试注入时钟。
func (s *Signer) SetNow(now func() time.Time) { s.now = now }

// Do 发送带签名的请求。rawURL 必须是目标的最终 URL（含 raw query）。
// body 为实际发送的字节；非 GET 时作为请求体发送。
func (s *Signer) Do(ctx context.Context, method, rawURL string, body []byte, extraHeaders map[string]string) (*http.Response, error) {
	if s.cred == nil {
		return nil, fmt.Errorf("httpsign: no credential")
	}
	cred := s.cred.Snapshot()
	nodeID := cred.NodeID
	keyID := cred.KeyID
	if nodeID == "" || keyID == "" {
		return nil, fmt.Errorf("httpsign: node identity is not enrolled")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("httpsign: parse url: %w", err)
	}
	pathWithQuery := parsed.EscapedPath()
	if parsed.RawQuery != "" {
		pathWithQuery += "?" + parsed.RawQuery
	}
	timestamp := strconv.FormatInt(s.now().UTC().UnixMilli(), 10)
	nonce, err := s.nonce()
	if err != nil {
		return nil, err
	}
	bodyHash := protocol.BodySHA256(body)
	fields := protocol.HTTPFields(s.audience, nodeID, keyID, method, pathWithQuery, bodyHash, timestamp, nonce)
	// 严格使用 token 绑定的 key 签名；pending 轮换 key 仅在服务端已接受它（token 绑定）时
	// 允许用于恢复查询，绝不降级到其他 key。
	signature, err := s.identity.SignWithCredential(keyID, fields...)
	if err != nil {
		return nil, fmt.Errorf("httpsign: sign with token-bound key %q: %w", keyID, err)
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), rawURL, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("X-Judge-Node-Id", nodeID)
	req.Header.Set("X-Judge-Key-Id", keyID)
	req.Header.Set("X-Judge-Timestamp", timestamp)
	req.Header.Set("X-Judge-Nonce", nonce)
	req.Header.Set("X-Judge-Signature", signature)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range extraHeaders {
		req.Header.Set(name, value)
	}
	return s.client.Do(req)
}

// SignedRequest 只构造并签名请求头，不发送，供测试断言规范化字段。
func (s *Signer) SignedRequest(ctx context.Context, method, rawURL string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), rawURL, reader)
	if err != nil {
		return nil, err
	}
	if err := s.applyHeaders(req, body); err != nil {
		return nil, err
	}
	return req, nil
}

func (s *Signer) applyHeaders(req *http.Request, body []byte) error {
	if s.cred == nil {
		return fmt.Errorf("httpsign: no credential")
	}
	cred := s.cred.Snapshot()
	nodeID := cred.NodeID
	keyID := cred.KeyID
	pathWithQuery := req.URL.EscapedPath()
	if req.URL.RawQuery != "" {
		pathWithQuery += "?" + req.URL.RawQuery
	}
	timestamp := strconv.FormatInt(s.now().UTC().UnixMilli(), 10)
	nonce, err := s.nonce()
	if err != nil {
		return err
	}
	fields := protocol.HTTPFields(s.audience, nodeID, keyID, req.Method, pathWithQuery, protocol.BodySHA256(body), timestamp, nonce)
	signature, err := s.identity.SignWithCredential(keyID, fields...)
	if err != nil {
		return fmt.Errorf("httpsign: sign with token-bound key %q: %w", keyID, err)
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("X-Judge-Node-Id", nodeID)
	req.Header.Set("X-Judge-Key-Id", keyID)
	req.Header.Set("X-Judge-Timestamp", timestamp)
	req.Header.Set("X-Judge-Nonce", nonce)
	req.Header.Set("X-Judge-Signature", signature)
	return nil
}

// ClassifyStatus 把 401/403 映射为不可重试的 DeniedError，其他非 2xx 视为可重试。
// 不把响应体原样带进错误，避免可能含 secret 的内容进入日志。
func ClassifyStatus(statusCode int, _ []byte) error {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return &auth.DeniedError{StatusCode: statusCode}
	}
	if statusCode < 200 || statusCode >= 300 {
		return fmt.Errorf("signed https request failed with status %d", statusCode)
	}
	return nil
}

func randomNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
