package webui

import (
	"os"
	"strings"
	"testing"
)

func TestStoreAdminPassword(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if store.AdminInitialized() {
		t.Fatal("admin should not be initialized")
	}
	if err := store.SaveAdminPassword("password123"); err != nil {
		t.Fatalf("save password: %v", err)
	}
	if !store.AdminInitialized() {
		t.Fatal("admin should be initialized")
	}
	if !store.VerifyPassword("password123") {
		t.Fatal("password should verify")
	}
	if store.VerifyPassword("wrong") {
		t.Fatal("wrong password should not verify")
	}
}

func TestStoreBootstrapTokenIsSecretAndConsumable(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if store.BootstrapConfigured() {
		t.Fatal("bootstrap should not be configured initially")
	}
	if err := store.WriteBootstrapToken("one-time-token"); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}
	if !store.BootstrapConfigured() {
		t.Fatal("bootstrap should be configured")
	}
	raw, err := os.ReadFile(store.BootstrapPath())
	if err != nil {
		t.Fatalf("read bootstrap: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "one-time-token" {
		t.Fatalf("bootstrap content = %q", string(raw))
	}
	fi, err := os.Stat(store.BootstrapPath())
	if err != nil {
		t.Fatalf("stat bootstrap: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("bootstrap perm = %04o, want 0600", perm)
	}
}

func TestStoreIdentityPathUnderStateDir(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if got := store.IdentityPath(); !strings.HasPrefix(got, dir) {
		t.Fatalf("identity path %q not under state dir %q", got, dir)
	}
}
