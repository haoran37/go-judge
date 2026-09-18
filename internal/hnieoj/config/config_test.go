package config

import (
	"strings"
	"testing"
	"time"
)

func TestValidateBackendURLRejectsRemotePlaintext(t *testing.T) {
	cases := []struct {
		raw     string
		wantErr bool
	}{
		{"https://oj.example.com", false},
		{"https://oj.example.com:8443/api", false},
		{"http://localhost:8800", false},
		{"http://127.0.0.1:8800", false},
		{"http://[::1]:8800", false},
		{"http://10.0.0.5:8800", true},
		{"http://oj.example.com", true},
		{"ftp://oj.example.com", true},
		{"oj.example.com", true},
	}
	for _, tc := range cases {
		err := ValidateBackendURL(tc.raw)
		if tc.wantErr && err == nil {
			t.Fatalf("ValidateBackendURL(%q) = nil, want error", tc.raw)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("ValidateBackendURL(%q) error: %v", tc.raw, err)
		}
	}
}

func TestValidateRejectsPlaintextRemoteBackend(t *testing.T) {
	cfg := defaultConfig()
	cfg.HnieOJ.BaseURL = "http://10.0.0.9:8800"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("expected https requirement error, got %v", err)
	}
}

func TestValidateDefaultsWorkerAndReporter(t *testing.T) {
	cfg := defaultConfig()
	cfg.HnieOJ.BaseURL = "https://oj.example.com"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Worker.EmptyMinBackoff != 200*time.Millisecond || cfg.Worker.EmptyMaxBackoff != 5*time.Second {
		t.Fatalf("unexpected worker backoff: %+v", cfg.Worker)
	}
	if cfg.Worker.DrainTimeout != 5*time.Minute {
		t.Fatalf("unexpected drain timeout: %v", cfg.Worker.DrainTimeout)
	}
	if cfg.Reporter.MaxRetries != 3 || cfg.Reporter.RetryBackoff != 2*time.Second {
		t.Fatalf("unexpected reporter retry config: %+v", cfg.Reporter)
	}
}

func TestValidateRequiresTempAuthCodeOrCredential(t *testing.T) {
	cfg := defaultConfig()
	cfg.Node.Type = "temp"
	cfg.HnieOJ.BaseURL = "https://oj.example.com"
	cfg.HnieOJ.Credential.TokenFile = ""
	cfg.HnieOJ.Credential.Token = ""
	cfg.HnieOJ.Credential.AuthCode = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected credential requirement error for temp node without credential")
	}
	cfg.HnieOJ.Credential.AuthCode = "code"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("temp node with authCode should validate: %v", err)
	}
}

func TestExampleConfigsLoad(t *testing.T) {
	for _, path := range []string{
		"../../../config.example.yaml",
		"../../../deploy/config.formal.example.yaml",
		"../../../deploy/config.temp.example.yaml",
	} {
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%s) error: %v", path, err)
		}
		if cfg.Node.MaxConcurrency <= 0 || len(cfg.Node.SupportedJudgeModes) == 0 {
			t.Fatalf("Load(%s) produced incomplete config: %+v", path, cfg.Node)
		}
	}
}

func TestNormalizeJudgeModes(t *testing.T) {
	got, err := normalizeJudgeModes([]string{" default ", "SPJ", "spj", "interactive"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"default", "spj", "interactive"}
	if len(got) != len(want) {
		t.Fatalf("modes = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("modes = %#v, want %#v", got, want)
		}
	}
	if _, err := normalizeJudgeModes([]string{"unsafe"}); err == nil {
		t.Fatal("expected unsupported mode error")
	}
}
