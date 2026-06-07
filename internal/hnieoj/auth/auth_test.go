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
	"runtime"
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

func TestTempTokenCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temp-token.json")
	cache := TempTokenCache{
		Token:      "jwt-value",
		TokenType:  "Bearer",
		NodeID:     "node-1",
		TokenID:    "token-1",
		ExpireTime: time.Now().Add(time.Hour).Format(time.RFC3339),
	}
	if err := SaveTempTokenCache(path, cache); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected cache permission: %v", info.Mode().Perm())
	}
	cred, err := LoadTempTokenCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if cred.HeaderName != "Authorization" || cred.HeaderValue != "Bearer jwt-value" ||
		cred.NodeID != "node-1" || cred.TokenID != "token-1" {
		t.Fatalf("unexpected credential: %+v", cred)
	}
}

func TestExpiredTempTokenCacheRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temp-token.json")
	cache := TempTokenCache{
		Token:      "jwt-value",
		TokenType:  "Bearer",
		ExpireTime: time.Now().Add(-time.Minute).Format(time.RFC3339),
	}
	if err := SaveTempTokenCache(path, cache); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTempTokenCache(path); err == nil {
		t.Fatal("expected expired temp token cache to be rejected")
	}
}
