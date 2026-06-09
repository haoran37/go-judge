package webui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/node"
)

func TestServerAdminSetupAndAuth(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	manager := node.NewManager(logging.NopLogger{})
	server := httptest.NewServer(NewServer(store, manager, logging.NewRecorder(nil, 10)).Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("config before setup status = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp, err = http.Post(server.URL+"/api/v1/setup/admin", "application/json", bytes.NewBufferString(`{"password":"password123"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin setup status = %d", resp.StatusCode)
	}
	cookies := resp.Cookies()
	_ = resp.Body.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config after setup status = %d, want 200", resp.StatusCode)
	}
}
