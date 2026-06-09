package webui

import "testing"

func TestStoreAdminPassword(t *testing.T) {
	store := NewStore(t.TempDir())
	if store.AdminInitialized() {
		t.Fatal("admin should not be initialized")
	}
	if err := store.SaveAdminPassword("password123"); err != nil {
		t.Fatal(err)
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

func TestStoreEnsureTempIdentityReusesFiles(t *testing.T) {
	store := NewStore(t.TempDir())
	id1, secret1, err := store.EnsureTempIdentity()
	if err != nil {
		t.Fatal(err)
	}
	id2, secret2, err := store.EnsureTempIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if id1 == "" || secret1 == "" {
		t.Fatal("identity should be generated")
	}
	if id1 != id2 || secret1 != secret2 {
		t.Fatalf("identity should be reused: %q/%q %q/%q", id1, id2, secret1, secret2)
	}
}
