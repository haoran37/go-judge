package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
)

func fakeJWT(expMillis int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"tokenId":"t","nodeId":"n"}`, expMillis)))
	return header + "." + payload + ".signature"
}

func TestJWTSchedulingExpiryIsTimezoneIndependent(t *testing.T) {
	exp := time.Date(2030, 3, 4, 5, 6, 7, 0, time.UTC)
	token := fakeJWT(exp.UnixMilli())
	got := jwtExpiryMillis(token)
	if got != exp.UnixMilli() {
		t.Fatalf("jwtExpiryMillis = %d, want %d", got, exp.UnixMilli())
	}
	// 秒级 exp 也应按秒处理。
	if got := jwtExpiryMillis(fakeJWT(exp.Unix())); got != exp.UnixMilli() {
		t.Fatalf("seconds exp = %d, want %d", got, exp.UnixMilli())
	}
}

func TestNextRenewDelayUsesJWTExpNotLocalExpireTime(t *testing.T) {
	now := time.Date(2030, 3, 4, 5, 0, 0, 0, time.UTC)
	exp := now.Add(10 * time.Minute)
	cred := &Credential{
		expireAtMillis: exp.UnixMilli(),
		// 本地字符串过期时间故意设成另一个时区的“过去”，不应影响调度。
		ExpireTime: now.Add(-48 * time.Hour),
	}
	got := nextRenewDelay(cred, 30*time.Second, now)
	if got != 9*time.Minute+30*time.Second {
		t.Fatalf("nextRenewDelay = %v, want 9m30s", got)
	}
	if nextRenewDelay(cred, 30*time.Second, exp) != 0 {
		t.Fatal("expected immediate renewal at expiry")
	}
	// exp 未知时退化为安全余量轮询，不能紧凑自旋。
	if got := nextRenewDelay(&Credential{}, 30*time.Second, now); got != 30*time.Second {
		t.Fatalf("unknown expiry delay = %v, want 30s", got)
	}
}

func TestLoadUsesExistingCredentialFileWithoutExchangingAuthCode(t *testing.T) {
	exchanges := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges++
		writeEnvelope(t, w, 200, map[string]any{"token": "unused"})
	}))
	defer server.Close()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "credential.json")
	exp := time.Now().Add(time.Hour).UnixMilli()
	token := fakeJWT(exp)
	if err := writeCredentialFile(tokenFile, storedCredential{
		NodeID: "node-1", TokenID: "token-1", NodeType: "temp",
		TokenType: "Bearer", Token: token, ExpireAtMillis: exp,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		Node:   config.NodeConfig{Name: "n", Type: "temp"},
		HnieOJ: config.HnieOJConfig{BaseURL: server.URL, Credential: config.Credential{TokenFile: tokenFile, AuthCode: "should-not-be-used"}},
	}
	m, err := Load(context.Background(), cfg, server.Client(), logging.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if exchanges != 0 {
		t.Fatalf("existing credential file should not trigger authCode exchange, got %d calls", exchanges)
	}
	if m.Credential().NodeID != "node-1" || m.Credential().TokenID != "token-1" || m.Credential().ExpireAtMillis() != exp {
		t.Fatalf("unexpected credential: %+v", m.Credential().Snapshot())
	}
}

func TestTempFirstEnrollmentPersistsAtomic0600AndRenewKeepsIdentity(t *testing.T) {
	var renewAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/judge/temp-token":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["authCode"] != "auth-code" {
				t.Errorf("authCode = %q", body["authCode"])
			}
			writeEnvelope(t, w, 200, map[string]any{
				"token": fakeJWT(time.Now().Add(time.Hour).UnixMilli()), "tokenType": "Bearer",
				"nodeId": "node-1", "tokenId": "token-1", "expireTime": "2030-01-01T00:00:00",
			})
		case "/judge/nodes/token/renew":
			renewAuth = r.Header.Get("Authorization")
			writeEnvelope(t, w, 200, map[string]any{
				"token": fakeJWT(time.Now().Add(2 * time.Hour).UnixMilli()), "tokenType": "Bearer",
				"nodeId": "node-1", "tokenId": "token-1", "expireTime": "2030-01-01T00:00:00",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tokenFile := filepath.Join(t.TempDir(), "cred.json")
	cfg := config.Config{
		Node: config.NodeConfig{Name: "n", Type: "temp"},
		HnieOJ: config.HnieOJConfig{
			BaseURL:    server.URL,
			Credential: config.Credential{TokenFile: tokenFile, AuthCode: "auth-code"},
		},
	}
	m, err := Load(context.Background(), cfg, server.Client(), logging.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file mode = %o, want 600", info.Mode().Perm())
	}
	stored, err := readCredentialFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if stored.NodeID != "node-1" || stored.TokenID != "token-1" || stored.Token == "" {
		t.Fatalf("unexpected stored credential: %+v", stored)
	}

	next, err := m.renew(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if renewAuth == "" || next.NodeID != "node-1" || next.TokenID != "token-1" {
		t.Fatalf("renew kept identity? auth=%q next=%+v", renewAuth, next.Snapshot())
	}
}

func TestRenewRejectsIdentityDrift(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, 200, map[string]any{
			"token": fakeJWT(time.Now().Add(time.Hour).UnixMilli()), "tokenType": "Bearer",
			"nodeId": "other-node", "tokenId": "token-1",
		})
	}))
	defer server.Close()

	m := &Manager{
		cfg:    config.Config{HnieOJ: config.HnieOJConfig{BaseURL: server.URL}},
		client: server.Client(), logger: logging.NopLogger{},
		cred: &Credential{
			NodeID: "node-1", TokenID: "token-1", HeaderName: "Authorization",
			HeaderValue: "Bearer " + fakeJWT(time.Now().Add(time.Hour).UnixMilli()),
		},
	}
	if _, err := m.renew(context.Background()); err == nil {
		t.Fatal("expected nodeId drift to be rejected")
	}
}

func TestDoJSONClassifiesDeniedAndMalformed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/denied":
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 403, "msg": "revoked"})
		case "/api-denied":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 403, "msg": "revoked"})
		case "/malformed":
			_, _ = w.Write([]byte("not-json"))
		default:
			_, _ = w.Write([]byte(`{"code":200,"msg":"ok","data":{"token":"x"}}`))
		}
	}))
	defer server.Close()

	m := &Manager{cfg: config.Config{HnieOJ: config.HnieOJConfig{BaseURL: server.URL}}, client: server.Client()}
	if _, err := m.doJSON(context.Background(), http.MethodPost, server.URL+"/denied", []byte("{}"), nil); !IsDenied(err) {
		t.Fatalf("expected denied error, got %v", err)
	}
	if _, err := m.doJSON(context.Background(), http.MethodPost, server.URL+"/api-denied", []byte("{}"), nil); !IsDenied(err) {
		t.Fatalf("expected api-denied error, got %v", err)
	}
	if _, err := m.doJSON(context.Background(), http.MethodPost, server.URL+"/malformed", []byte("{}"), nil); err == nil || IsDenied(err) {
		t.Fatalf("expected malformed error, got %v", err)
	}
}

func writeEnvelope(t *testing.T, w http.ResponseWriter, code int, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "msg": "success", "data": data})
}

// TestFixedTempExpiryDoesNotBusyRenew 覆盖 temp 节点后端无法推进过期时间
// （受授权期上限约束）时，续期必须停止空转，并且不能过早撤销仍然有效的凭证。
func TestFixedTempExpiryDoesNotBusyRenew(t *testing.T) {
	exp := time.Now().Add(time.Second).UnixMilli()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeEnvelope(t, w, 200, map[string]any{
			"token": fakeJWT(exp), "tokenType": "Bearer", "nodeId": "n", "tokenId": "t",
		})
	}))
	defer server.Close()

	cfg := config.Config{
		Node: config.NodeConfig{Type: "temp"},
		HnieOJ: config.HnieOJConfig{
			BaseURL: server.URL,
			Renew:   config.RenewConfig{SafetyMargin: 2 * time.Second, RetryBackoff: 100 * time.Millisecond},
		},
	}
	manager := &Manager{cfg: cfg, client: server.Client(), logger: logging.NopLogger{}, cred: credentialFromToken(fakeJWT(exp), "temp", "n", "t")}
	ctx, cancel := context.WithTimeout(context.Background(), 220*time.Millisecond)
	defer cancel()
	manager.renewLoop(ctx)

	if count := calls.Load(); count > 4 {
		t.Fatalf("unchanged authorized expiry caused %d renewals in 220ms", count)
	}
	if manager.Credential().Revoked() {
		t.Fatal("still-valid fixed temp credential must not be marked revoked")
	}
	if manager.Credential().Expired(time.Now()) {
		t.Fatal("fixed temp credential expired before its backend expiry")
	}
}

// TestAdvancingShortTTLStaysAliveAcrossRenewals 覆盖 formal 短 TTL：后端每次续期都
// 推进有效期，并且拒绝已经过期的请求；节点必须在下一次过期前及时续期，
// 不能睡过新的过期时间而丢失凭证。
func TestAdvancingShortTTLStaysAliveAcrossRenewals(t *testing.T) {
	var allowedUntil atomic.Int64
	allowedUntil.Store(time.Now().Add(200 * time.Millisecond).UnixMilli())
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if time.Now().UnixMilli() >= allowedUntil.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		calls.Add(1)
		expiry := time.Now().Add(200 * time.Millisecond).UnixMilli()
		allowedUntil.Store(expiry)
		writeEnvelope(t, w, 200, map[string]any{
			"token": fakeJWT(expiry), "tokenType": "Bearer", "nodeId": "n", "tokenId": "t",
		})
	}))
	defer server.Close()

	cfg := config.Config{
		Node: config.NodeConfig{Type: "formal"},
		HnieOJ: config.HnieOJConfig{
			BaseURL: server.URL,
			Renew:   config.RenewConfig{SafetyMargin: 2 * time.Second, RetryBackoff: 20 * time.Millisecond},
		},
	}
	manager := &Manager{cfg: cfg, client: server.Client(), logger: logging.NopLogger{}, cred: credentialFromToken(fakeJWT(allowedUntil.Load()), "formal", "n", "t")}
	ctx, cancel := context.WithTimeout(context.Background(), 650*time.Millisecond)
	defer cancel()
	manager.renewLoop(ctx)

	if manager.Credential().Revoked() {
		t.Fatal("renewable short-TTL credential must not be marked revoked")
	}
	if manager.Credential().Expired(time.Now()) {
		t.Fatal("node slept past the renewed short TTL and lost the credential")
	}
	if count := calls.Load(); count < 2 {
		t.Fatalf("short TTL produced only %d timely renewals, want several periods", count)
	}
}

// TestExpiredUnrenewableCredentialStopsRenewal 覆盖凭证已实际过期且后端无法推进时，
// 续期循环应停止并标记撤销，而不是无限重试。
func TestExpiredUnrenewableCredentialStopsRenewal(t *testing.T) {
	exp := time.Now().Add(-time.Second).UnixMilli()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeEnvelope(t, w, 200, map[string]any{
			"token": fakeJWT(exp), "tokenType": "Bearer", "nodeId": "n", "tokenId": "t",
		})
	}))
	defer server.Close()

	cfg := config.Config{
		Node: config.NodeConfig{Type: "temp"},
		HnieOJ: config.HnieOJConfig{
			BaseURL: server.URL,
			Renew:   config.RenewConfig{SafetyMargin: 30 * time.Second, RetryBackoff: 10 * time.Millisecond},
		},
	}
	manager := &Manager{cfg: cfg, client: server.Client(), logger: logging.NopLogger{}, cred: credentialFromToken(fakeJWT(exp), "temp", "n", "t")}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.renewLoop(ctx)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("renewLoop kept running for an expired, non-extendable credential")
	}
	if !manager.Credential().Revoked() {
		t.Fatal("expired non-extendable credential should be marked revoked")
	}
	if count := calls.Load(); count > 1 {
		t.Fatalf("expired credential renewed %d times, want at most 1", count)
	}
}
