package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
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
	next := &Credential{HeaderName: "Authorization", HeaderValue: "Bearer new", NodeID: "new", TokenID: "token-2", ExpireTime: time.Now().Add(time.Hour)}

	cred.Replace(next)

	if cred.HeaderValue != "Bearer new" || cred.NodeID != "new" || cred.TokenID != "token-2" || cred.ExpireTime.IsZero() {
		t.Fatalf("credential was not replaced: %+v", cred)
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
