package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/node"
	"go.uber.org/zap"
)

func testServer(t *testing.T) (*Server, *Store, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	manager := node.NewManager(logging.NopLogger{})
	recorder := logging.NewRecorder(zap.NewNop(), 10)
	server := NewServer(store, manager, recorder)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return server, store, ts
}

func setupAdminSession(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar}
	resp, err := client.Post(ts.URL+"/api/v1/setup/admin", "application/json", bytes.NewReader([]byte(`{"password":"password123"}`)))
	if err != nil {
		t.Fatalf("setup admin: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("setup admin status %d: %s", resp.StatusCode, body)
	}
	return client
}

func TestConfigDTOHidesSecrets(t *testing.T) {
	_, store, ts := testServer(t)
	client := setupAdminSession(t, ts)
	if err := store.WriteBootstrapToken("super-secret-bootstrap"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	resp, err := client.Get(ts.URL + "/api/v1/config")
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	for _, secret := range []string{"super-secret-bootstrap", "privateKey", "accessToken", "private_key"} {
		if strings.Contains(text, secret) {
			t.Fatalf("config response leaked %q: %s", secret, text)
		}
	}
	var dto ConfigDTO
	if err := json.Unmarshal(raw, &dto); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if !dto.Identity.BootstrapConfigured {
		t.Fatal("bootstrapConfigured flag should be true")
	}
	if dto.Identity.BootstrapToken != "" {
		t.Fatal("bootstrapToken must never be returned")
	}
}

func TestSetupBootstrapWritesSecretAndConfig(t *testing.T) {
	_, store, ts := testServer(t)
	client := setupAdminSession(t, ts)

	payload := ConfigDTO{
		Node:     NodeDTO{Name: "node-a", Type: "formal", MaxConcurrency: 2, SupportedJudgeModes: []string{"default"}},
		HnieOJ:   HnieOJDTO{BaseURL: "https://oj.example.com", RequestTimeout: "30s"},
		Identity: IdentityDTO{BootstrapToken: "one-time-bootstrap"},
		Rotation: RotationDTO{Enabled: true, Interval: "720h", Grace: "5m", ConfirmTimeout: "30s"},
	}
	body, _ := json.Marshal(map[string]any{"config": payload})
	resp, err := client.Post(ts.URL+"/api/v1/setup/bootstrap", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("setup bootstrap: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("setup bootstrap status %d: %s", resp.StatusCode, raw)
	}
	if !store.BootstrapConfigured() {
		t.Fatal("bootstrap token file should exist")
	}
	raw, err := os.ReadFile(store.BootstrapPath())
	if err != nil {
		t.Fatalf("read bootstrap: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "one-time-bootstrap" {
		t.Fatalf("bootstrap content %q", raw)
	}
	cfg, ok, err := store.LoadConfig()
	if err != nil || !ok {
		t.Fatalf("load config ok=%v err=%v", ok, err)
	}
	if cfg.Node.Name != "node-a" || cfg.Identity.File == "" {
		t.Fatalf("config not persisted correctly: %+v", cfg)
	}
	if cfg.Bootstrap.Token != "" {
		t.Fatal("bootstrap token must not be persisted in config.yaml")
	}
	if cfg.HnieOJ.WSSURL == "" {
		t.Fatal("wss url should be resolved on validate")
	}
}

func TestSetupBootstrapWithoutTokenIsRejected(t *testing.T) {
	_, _, ts := testServer(t)
	client := setupAdminSession(t, ts)
	payload := ConfigDTO{
		Node:     NodeDTO{Name: "node-a", Type: "formal", MaxConcurrency: 1, SupportedJudgeModes: []string{"default"}},
		HnieOJ:   HnieOJDTO{BaseURL: "https://oj.example.com"},
		Rotation: RotationDTO{Enabled: true},
	}
	body, _ := json.Marshal(map[string]any{"config": payload})
	resp, err := client.Post(ts.URL+"/api/v1/setup/bootstrap", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestStaticHandlerFallsBackForSPARoutesOnly(t *testing.T) {
	handler := StaticHandler()
	for _, route := range []string{"/dashboard", "/configure/formal", "/logs"} {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("SPA route %s status %d", route, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<!") && !strings.Contains(rec.Body.String(), "html") {
			t.Fatalf("SPA route %s did not serve index", route)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/definitely-missing", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown non-SPA route status %d", rec.Code)
	}
}
