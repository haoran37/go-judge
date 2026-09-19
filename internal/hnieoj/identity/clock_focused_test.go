package identity

import (
	"path/filepath"
	"testing"
	"time"
)

// focusedGraceStore 构造一个旧 key 处于 GRACE、grace 截止为 graceUntil 的身份。
func focusedGraceStore(t *testing.T, graceUntil int64) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "identity.json"), "n", "formal")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginRotation("rotation-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRotationPrepared("rotation-1", "key-2", "nonce", time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRotationConfirmed(graceUntil); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateRotatedKey(); err != nil {
		t.Fatal(err)
	}
	return store
}

// server 已判定 grace 过期、本地时钟落后时，不能因本地时钟而继续接受旧 key。
func TestFocusedGraceExpiryUsesInjectedServerClock(t *testing.T) {
	serverNow := time.Now().Add(2 * time.Hour)
	store := focusedGraceStore(t, serverNow.Add(-time.Second).UnixMilli())
	store.SetNow(func() time.Time { return serverNow })
	if _, ok := store.PrivateKeyByID("key-1"); ok {
		t.Fatal("expired grace key accepted under injected server clock")
	}
	if store.HasSigningKey("key-1") {
		t.Fatal("expired grace key reported signable under injected server clock")
	}
}

// server 认为 grace 仍有效、本地时钟超前时，旧 key 仍应可签名。
func TestFocusedValidGraceUsesInjectedServerClockOverLocal(t *testing.T) {
	serverNow := time.Now().Add(-2 * time.Hour)
	store := focusedGraceStore(t, serverNow.Add(60*time.Second).UnixMilli())
	store.SetNow(func() time.Time { return serverNow })
	if _, ok := store.PrivateKeyByID("key-1"); !ok {
		t.Fatal("grace key valid under server clock rejected because local clock is ahead")
	}
	if _, err := store.SignWith("key-1", "x"); err != nil {
		t.Fatalf("grace key should sign under server clock: %v", err)
	}
}
