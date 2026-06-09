package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/config"
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

func TestServerSessionCookieExpiresInTwoHours(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	manager := node.NewManager(logging.NopLogger{})
	server := httptest.NewServer(NewServer(store, manager, logging.NewRecorder(nil, 10)).Handler())
	defer server.Close()

	start := time.Now()
	resp, err := http.Post(server.URL+"/api/v1/setup/admin", "application/json", bytes.NewBufferString(`{"password":"password123"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin setup status = %d", resp.StatusCode)
	}
	cookie := findCookie(resp.Cookies(), sessionCookie)
	if cookie == nil {
		t.Fatalf("missing %s cookie", sessionCookie)
	}
	minExpires := start.Add(sessionTTL - 2*time.Second)
	maxExpires := time.Now().Add(sessionTTL + 2*time.Second)
	if cookie.Expires.Before(minExpires) || cookie.Expires.After(maxExpires) {
		t.Fatalf("session expires at %s, want around %s", cookie.Expires, start.Add(sessionTTL))
	}
}

func TestStaticHandlerFallsBackForSPARoutesOnly(t *testing.T) {
	server := httptest.NewServer(StaticHandler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body.String(), `id="app"`) {
		t.Fatal("dashboard route did not return SPA shell")
	}

	resp, err = http.Get(server.URL + "/api/v1/missing")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("api missing status = %d, want 404", resp.StatusCode)
	}

	resp, err = http.Get(server.URL + "/missing-page")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing page status = %d, want 404", resp.StatusCode)
	}
}

func TestSetupFormalPreservesStoredSecretsWhenFieldsAreBlank(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAdminPassword("password123"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.HnieOJ.BaseURL = "http://hnieoj.example"
	cfg.HnieOJ.FormalToken.PrivateKeyPath = "/existing/private.pem"
	cfg.RabbitMQ.Password = "rabbit-secret"
	if err := store.SaveConfig(*cfg); err != nil {
		t.Fatal(err)
	}
	manager := node.NewManager(logging.NopLogger{})
	manager.SetConfig(*cfg)
	server := httptest.NewServer(NewServer(store, manager, logging.NewRecorder(nil, 10)).Handler())
	defer server.Close()

	cookie := loginForTest(t, server.URL)
	dto := configToDTO(*cfg)
	dto.Node.Name = "judge-node-updated"
	dto.RabbitMQ.Password = ""
	payload, err := json.Marshal(map[string]any{
		"config":        dto,
		"privateKeyPem": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/setup/formal", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(resp.Body)
		t.Fatalf("setup formal status = %d, body = %s", resp.StatusCode, body.String())
	}

	saved, ok, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("config was not saved")
	}
	if saved.HnieOJ.FormalToken.PrivateKeyPath != "/existing/private.pem" {
		t.Fatalf("private key path = %q, want existing path", saved.HnieOJ.FormalToken.PrivateKeyPath)
	}
	if saved.RabbitMQ.Password != "rabbit-secret" {
		t.Fatalf("rabbit password = %q, want preserved secret", saved.RabbitMQ.Password)
	}
	if saved.Node.Name != "judge-node-updated" {
		t.Fatalf("node name = %q, want updated name", saved.Node.Name)
	}
}

func loginForTest(t *testing.T, serverURL string) *http.Cookie {
	t.Helper()
	resp, err := http.Post(serverURL+"/api/v1/auth/login", "application/json", bytes.NewBufferString(`{"password":"password123"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	cookie := findCookie(resp.Cookies(), sessionCookie)
	if cookie == nil {
		t.Fatalf("missing %s cookie", sessionCookie)
	}
	return cookie
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}
