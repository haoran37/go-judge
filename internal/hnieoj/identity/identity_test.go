package identity

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOpenGeneratesRealRandomKeyAndPersistsBeforeEnrollment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "identity.json")
	store, err := Open(path, "node-a", "formal")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	pub := store.Public()
	if pub.EnrollmentID == "" {
		t.Fatal("enrollmentId must be set immediately")
	}
	if pub.PublicKey == "" || pub.KeyStatus != KeyActive {
		t.Fatalf("unexpected public identity: %+v", pub)
	}
	if store.NodeID() != "" || store.CurrentKeyID() != "" {
		t.Fatal("nodeId/keyId must be empty before enrollment")
	}
	if _, err := store.CurrentPrivateKey(); err != nil {
		t.Fatalf("private key: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("identity file perms = %04o, want 0600", perm)
		}
		if dirInfo, err := os.Stat(filepath.Dir(path)); err != nil {
			t.Fatalf("stat dir: %v", err)
		} else if perm := dirInfo.Mode().Perm(); perm != 0o700 {
			t.Fatalf("identity dir perms = %04o, want 0700", perm)
		}
	}

	// 重启只复用同一 enrollmentId 与 keypair，不生成新身份。
	reopened, err := Open(path, "node-a", "formal")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Public().EnrollmentID != pub.EnrollmentID {
		t.Fatal("enrollmentId changed across restart")
	}
	if reopened.Public().PublicKey != pub.PublicKey {
		t.Fatal("public key changed across restart")
	}
}

func TestPublicOmitsPrivateKeyAndBootstrap(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "identity.json"), "n", "temp")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatalf("apply enrollment: %v", err)
	}
	rendered := strings.ToLower(strings.Join([]string{
		store.Public().EnrollmentID,
		store.Public().NodeID,
		store.Public().KeyID,
		store.Public().PublicKey,
	}, " "))
	if strings.Contains(rendered, "private") || strings.Contains(rendered, "seed") {
		t.Fatalf("public identity leaked secret material: %s", rendered)
	}
}

func TestApplyEnrollmentPersistsNodeAndKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	store, err := Open(path, "n", "formal")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.ApplyEnrollment("node-42", "key-42"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	reopened, err := Open(path, "n", "formal")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.NodeID() != "node-42" || reopened.CurrentKeyID() != "key-42" {
		t.Fatalf("enrollment not persisted: %s/%s", reopened.NodeID(), reopened.CurrentKeyID())
	}
}

func TestBeginRotationPersistsNewKeyBeforePrepare(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.json")
	store, err := Open(path, "n", "formal")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	oldPub := store.Public().PublicKey
	pending, err := store.BeginRotation("rotation-1")
	if err != nil {
		t.Fatalf("begin rotation: %v", err)
	}
	if pending.NewPublicKey == oldPub {
		t.Fatal("rotation must generate a fresh key")
	}

	// 模拟 prepare 响应丢失后崩溃：重新打开仍持有 pending 新密钥，且当前 key 未变。
	reopened, err := Open(path, "n", "formal")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Public().PublicKey != oldPub {
		t.Fatal("active key changed without confirmation")
	}
	pending2 := reopened.Pending()
	if pending2 == nil || pending2.RotationID != "rotation-1" || pending2.NewPublicKey != pending.NewPublicKey {
		t.Fatalf("pending rotation lost after crash: %+v", pending2)
	}
	if _, err := reopened.CurrentPrivateKey(); err != nil {
		t.Fatalf("active private key must survive: %v", err)
	}
}

func TestRotationConfirmSwitchesKeyAndKeepsOldInGrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	store, err := Open(path, "n", "formal")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	oldPub := store.Public().PublicKey
	if _, err := store.BeginRotation("rotation-1"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := store.RecordRotationPrepared("rotation-1", "key-2", "nonce", time.Now().Add(15*time.Minute).UnixMilli()); err != nil {
		t.Fatalf("record: %v", err)
	}
	graceUntil := time.Now().Add(5 * time.Minute).UnixMilli()
	if err := store.MarkRotationConfirmed(graceUntil); err != nil {
		t.Fatalf("mark confirmed: %v", err)
	}
	if err := store.ActivateRotatedKey(); err != nil {
		t.Fatalf("activate: %v", err)
	}
	pub := store.Public()
	if pub.KeyID != "key-2" {
		t.Fatalf("current key = %s, want key-2", pub.KeyID)
	}
	if pub.PublicKey == oldPub {
		t.Fatal("public key did not switch")
	}
	if pub.GraceKeyID != "key-1" || pub.GraceUntil != graceUntil {
		t.Fatalf("old key not in grace: %+v", pub)
	}
	if _, ok := store.PrivateKeyByID("key-1"); !ok {
		t.Fatal("grace key private material must be retained until grace expiry")
	}
	if _, err := store.SignWith("key-1", "x"); err != nil {
		t.Fatalf("grace key sign: %v", err)
	}

	// grace 到期后清理旧 key，但当前 key 必须保留。
	if changed, err := store.ExpireGrace(time.UnixMilli(graceUntil + 1)); err != nil || !changed {
		t.Fatalf("expire grace changed=%v err=%v", changed, err)
	}
	if _, ok := store.PrivateKeyByID("key-1"); ok {
		t.Fatal("expired grace key retained forever")
	}
	if _, err := store.CurrentPrivateKey(); err != nil {
		t.Fatalf("active key must survive grace cleanup: %v", err)
	}
}

func TestSignWithCredentialIsNarrowlyScopedToPendingKey(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "identity.json"), "n", "formal")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := store.BeginRotation("rotation-1"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := store.RecordRotationPrepared("rotation-1", "key-2", "nonce", time.Now().Add(time.Minute).UnixMilli()); err != nil {
		t.Fatalf("record: %v", err)
	}
	keyID := store.Pending().NewKeyID
	if keyID == "" {
		t.Fatal("prepared pending rotation missing keyId")
	}
	// 普通业务签名路径仍不得使用未本地激活的 pending key。
	if _, err := store.SignWith(keyID, "x"); err == nil {
		t.Fatal("SignWith accepted an unactivated pending key")
	}
	// 只有 token 绑定（服务端已接受）的 pending key 才允许恢复签名。
	if _, err := store.SignWithCredential(keyID, "x"); err != nil {
		t.Fatalf("token-bound pending key rejected: %v", err)
	}
	// 无关 key 一律拒绝，绝不回退到其他密钥。
	if _, err := store.SignWithCredential("ghost", "x"); err == nil {
		t.Fatal("unknown key accepted for signing")
	}
	if _, err := store.SignWithCredential("key-1", "x"); err != nil {
		t.Fatalf("active key rejected: %v", err)
	}
}

func TestLocalPrepareDeadlineRetainsPendingUntilAuthoritativeDiscard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	store, err := Open(path, "n", "formal")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	active := store.Public().PublicKey
	if _, err := store.BeginRotation("rotation-1"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := store.RecordRotationPrepared("rotation-1", "key-2", "nonce", time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatalf("record: %v", err)
	}
	// 本地 prepare 截止时间已过，但 confirm 结果可能已在服务端成功：绝不能据此删除新私钥。
	if changed, err := store.ExpirePendingRotation(time.Now()); err != nil || changed {
		t.Fatalf("local deadline expired pending changed=%v err=%v", changed, err)
	}
	if store.Pending() == nil {
		t.Fatal("uncertain confirm outcome discarded pending private key on local deadline")
	}
	if store.Public().PublicKey != active {
		t.Fatal("active key changed by local pending deadline")
	}
	// 只有服务端权威判定 EXPIRED 后才允许丢弃，且当前 ACTIVE key 必须保留。
	changed, err := store.DiscardPendingRotation(time.Now())
	if err != nil || !changed {
		t.Fatalf("authoritative discard changed=%v err=%v", changed, err)
	}
	if store.Pending() != nil {
		t.Fatal("authoritatively expired pending rotation not cleared")
	}
	if store.Public().PublicKey != active {
		t.Fatal("active key changed by pending discard")
	}
	if _, err := store.CurrentPrivateKey(); err != nil {
		t.Fatalf("active key lost: %v", err)
	}
}
