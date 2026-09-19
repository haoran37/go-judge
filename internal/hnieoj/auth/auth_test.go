package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestJWTExpiryReturnsMillis(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"exp": 1893456000})
	token := "h." + base64.RawURLEncoding.EncodeToString(payload) + ".s"
	if got := JWTExpiryMillis(token); got != 1893456000000 {
		t.Fatalf("JWTExpiryMillis = %d", got)
	}
	if got := JWTExpiryMillis("not-a-jwt"); got != 0 {
		t.Fatalf("malformed token exp = %d", got)
	}
}

func TestCredentialReplaceAndExpiry(t *testing.T) {
	cred := &Credential{}
	next := &Credential{
		NodeID: "node-1", KeyID: "key-1", AccessToken: "token",
		AccessExpiry: time.Now().Add(time.Minute).UnixMilli(),
		SessionEpoch: 7, MaxConcurrency: 3, SupportedJudgeModes: []string{"default"},
	}
	cred.Replace(next)
	if cred.NodeIDValue() != "node-1" || cred.KeyIDValue() != "key-1" {
		t.Fatalf("replace did not copy identity: %+v", cred.Snapshot())
	}
	if cred.SessionEpochValue() != 7 || cred.AccessTokenValue() != "token" {
		t.Fatalf("replace did not copy authorization: %+v", cred.Snapshot())
	}
	if cred.Expired(time.Now()) {
		t.Fatal("credential should not be expired")
	}
	if !cred.Expired(time.Now().Add(2 * time.Minute)) {
		t.Fatal("credential should be expired")
	}
	cred.MarkRevoked()
	if !cred.Revoked() {
		t.Fatal("revoked flag not set")
	}
	// Replace 清除 revoked，避免旧拒绝状态污染新授权。
	cred.Replace(next)
	if cred.Revoked() {
		t.Fatal("replace should clear revoked")
	}
}

func TestDeniedAndPermanentErrors(t *testing.T) {
	if !IsDenied(&DeniedError{StatusCode: 403}) {
		t.Fatal("DeniedError should be denied")
	}
	if !IsDenied(ErrIdentityRejected) {
		t.Fatal("ErrIdentityRejected should be denied")
	}
	if IsDenied(&PermanentError{Code: 400}) {
		t.Fatal("PermanentError is not a denial")
	}
	if !IsPermanent(&PermanentError{Code: 400}) {
		t.Fatal("PermanentError should be permanent")
	}
}
