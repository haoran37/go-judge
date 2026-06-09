package auth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/config"
)

func TestDecryptFormalToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	plain := "formal-token-value"
	cipherText, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &key.PublicKey, []byte(plain), nil)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "key.pem")
	keyBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}), 0o600); err != nil {
		t.Fatal(err)
	}

	encryptedToken := "{rsa}" + base64.StdEncoding.EncodeToString(cipherText)
	got, err := decryptFormalToken(config.FormalToken{
		PrivateKeyPath:  keyPath,
		CipherAlgorithm: "RSA/ECB/OAEPWithSHA-256AndMGF1Padding",
	}, encryptedToken)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain {
		t.Fatalf("got %q, want %q", got, plain)
	}
}

func TestParseExpireTimeWithoutZone(t *testing.T) {
	got, err := parseExpireTime("2026-05-12T14:00:00")
	if err != nil {
		t.Fatal(err)
	}
	if got.Year() != 2026 || got.Month() != time.May || got.Day() != 12 || got.Hour() != 14 {
		t.Fatalf("unexpected expire time: %v", got)
	}
}

func TestCredentialReplace(t *testing.T) {
	cred := &Credential{HeaderName: "Authorization", HeaderValue: "Bearer old", NodeID: "old"}
	next := &Credential{
		HeaderName:      "Authorization",
		HeaderValue:     "Bearer new",
		NodeID:          "new",
		TokenID:         "token-2",
		ExpireTime:      time.Now().Add(time.Hour),
		InstanceID:      "instance-1",
		FingerprintHash: "fingerprint",
		ProofType:       "hmac-sha256",
		InstanceSecret:  []byte("secret"),
	}

	cred.Replace(next)

	if cred.HeaderValue != "Bearer new" || cred.NodeID != "new" || cred.TokenID != "token-2" || cred.ExpireTime.IsZero() || cred.InstanceID != "instance-1" || cred.FingerprintHash != "fingerprint" || string(cred.InstanceSecret) != "secret" {
		t.Fatalf("credential was not replaced: %+v", cred)
	}
}

func TestCredentialFromTempTokenConfig(t *testing.T) {
	cred, err := credentialFromTempTokenConfig(config.TempToken{
		JWT:        "jwt-value",
		TokenType:  "Bearer",
		NodeID:     "node-id",
		TokenID:    "token-id",
		ExpireTime: "2026-05-12T14:00:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cred.HeaderName != "Authorization" || cred.HeaderValue != "Bearer jwt-value" || cred.NodeID != "node-id" || cred.TokenID != "token-id" || cred.ExpireTime.IsZero() {
		t.Fatalf("unexpected credential: %+v", cred)
	}
}

func TestTempRefreshDelayRefreshesBeforeExpiry(t *testing.T) {
	now := time.Date(2026, 6, 7, 10, 0, 0, 0, time.UTC)
	expire := now.Add(10 * time.Minute)
	if got := tempRefreshDelay(expire, now); got != 9*time.Minute {
		t.Fatalf("refresh delay = %v, want 9m", got)
	}
	if got := tempRefreshDelay(now.Add(30*time.Second), now); got != 0 {
		t.Fatalf("near-expiry refresh delay = %v, want 0", got)
	}
}

func TestCredentialApplySignsRequest(t *testing.T) {
	body := []byte(`{"ok":true}`)
	req, err := http.NewRequest(http.MethodPost, "http://example.com/judge/events?submissionId=1", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	cred := &Credential{
		HeaderName:      "Authorization",
		HeaderValue:     "Bearer jwt-value",
		NodeID:          "node-id",
		TokenID:         "token-id",
		InstanceID:      "instance-id",
		FingerprintHash: "fingerprint-hash",
		ProofType:       "hmac-sha256",
		InstanceSecret:  []byte("instance-secret"),
	}

	cred.Apply(req)

	if got := req.Header.Get("Authorization"); got != "Bearer jwt-value" {
		t.Fatalf("Authorization = %q", got)
	}
	for _, header := range []string{"X-Judge-Node-Id", "X-Judge-Token-Id", "X-Judge-Instance-Id", "X-Judge-Fingerprint", "X-Judge-Timestamp", "X-Judge-Nonce", "X-Judge-Body-Sha256", "X-Judge-Signature"} {
		if req.Header.Get(header) == "" {
			t.Fatalf("missing signed header %s", header)
		}
	}
	sum := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(sum[:])
	if got := req.Header.Get("X-Judge-Body-Sha256"); got != bodyHash {
		t.Fatalf("body hash = %q, want %q", got, bodyHash)
	}
	signingString := strings.Join([]string{
		http.MethodPost,
		"/judge/events?submissionId=1",
		bodyHash,
		req.Header.Get("X-Judge-Timestamp"),
		req.Header.Get("X-Judge-Nonce"),
	}, "\n")
	mac := hmac.New(sha256.New, []byte("instance-secret"))
	_, _ = mac.Write([]byte(signingString))
	wantSignature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if got := req.Header.Get("X-Judge-Signature"); got != wantSignature {
		t.Fatalf("signature = %q, want %q", got, wantSignature)
	}
	remaining, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(remaining, body) {
		t.Fatalf("request body was not preserved: %q", remaining)
	}
}

func TestExchangeTempTokenSendsBindingPayload(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "instance-secret")
	if err := os.WriteFile(secretPath, []byte("instance-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var got tempTokenRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/judge/temp-token" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"data": map[string]any{
				"token":           "jwt-value",
				"tokenType":       "Bearer",
				"nodeId":          "node-id",
				"tokenId":         "token-id",
				"expireTime":      "2026-05-12T14:00:00Z",
				"fingerprintHash": "backend-fingerprint",
			},
		})
	}))
	defer server.Close()

	cred, err := exchangeTempToken(context.Background(), config.Config{
		Node: config.NodeConfig{
			Name:                "temp-node",
			SupportedJudgeModes: []string{"default", "spj"},
		},
		HnieOJ: config.HnieOJConfig{
			BaseURL: server.URL,
			TempToken: config.TempToken{
				AuthCode:           "auth-code",
				InstanceID:         "instance-id",
				InstanceSecretPath: secretPath,
				ProofType:          "hmac-sha256",
			},
		},
	}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthCode != "auth-code" || got.NodeName != "temp-node" {
		t.Fatalf("unexpected token request: %+v", got)
	}
	if got.Fingerprint == nil || got.Fingerprint.InstanceID != "instance-id" || got.Fingerprint.NodeName != "temp-node" {
		t.Fatalf("missing fingerprint: %+v", got.Fingerprint)
	}
	if got.Proof == nil || got.Proof.Type != "hmac-sha256" || got.Proof.SecretHash == "" {
		t.Fatalf("missing proof: %+v", got.Proof)
	}
	if cred.FingerprintHash != "backend-fingerprint" || cred.InstanceID != "instance-id" || string(cred.InstanceSecret) != "instance-secret" {
		t.Fatalf("unexpected credential binding: %+v", cred)
	}
}
