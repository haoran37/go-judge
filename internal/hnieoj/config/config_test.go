package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateBackendURLRejectsRemotePlaintext(t *testing.T) {
	if err := ValidateBackendURL("http://oj.example.com"); err == nil {
		t.Fatal("remote plaintext backend must be rejected")
	}
	if err := ValidateBackendURL("https://oj.example.com"); err != nil {
		t.Fatalf("https backend: %v", err)
	}
	if err := ValidateBackendURL("http://127.0.0.1:8080"); err != nil {
		t.Fatalf("loopback http backend: %v", err)
	}
}

func TestValidateWSSURLRejectsRemotePlaintext(t *testing.T) {
	if err := ValidateWSSURL("ws://oj.example.com/ws/judge/node"); err == nil {
		t.Fatal("remote ws must be rejected")
	}
	if err := ValidateWSSURL("wss://oj.example.com/ws/judge/node"); err != nil {
		t.Fatalf("wss: %v", err)
	}
	if err := ValidateWSSURL("ws://127.0.0.1:8080/ws/judge/node"); err != nil {
		t.Fatalf("loopback ws: %v", err)
	}
}

func TestDeriveWSSURL(t *testing.T) {
	got, err := DeriveWSSURL("https://oj.example.com")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if got != "wss://oj.example.com/ws/judge/node" {
		t.Fatalf("derived %q", got)
	}
	got, err = DeriveWSSURL("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if got != "ws://127.0.0.1:8080/ws/judge/node" {
		t.Fatalf("derived %q", got)
	}
}

func TestValidateAppliesDefaultsAndIdentity(t *testing.T) {
	cfg := defaultConfig()
	cfg.HnieOJ.BaseURL = "https://oj.example.com"
	cfg.Identity.File = "/tmp/identity.json"
	cfg.Identity.StateDir = "/tmp/state"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.HnieOJ.WSSURL != "wss://oj.example.com/ws/judge/node" {
		t.Fatalf("wss url not derived: %q", cfg.HnieOJ.WSSURL)
	}
	if cfg.WSS.ControlFrameBytes != 64*1024 || cfg.WSS.TaskFrameBytes != 4*1024*1024 {
		t.Fatalf("frame defaults wrong: %+v", cfg.WSS)
	}
	if cfg.Rotation.Interval != 30*24*time.Hour {
		t.Fatalf("rotation interval default wrong: %s", cfg.Rotation.Interval)
	}
	if cfg.WSS.ResultQueueDir == "" {
		t.Fatal("result queue dir must default")
	}
}

func TestValidateRequiresIdentityFile(t *testing.T) {
	cfg := defaultConfig()
	cfg.HnieOJ.BaseURL = "https://oj.example.com"
	cfg.Identity.File = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("missing identity file must be rejected")
	}
}

func TestValidateRejectsOversizedFrames(t *testing.T) {
	cfg := defaultConfig()
	cfg.HnieOJ.BaseURL = "https://oj.example.com"
	cfg.Identity.File = "/tmp/identity.json"
	cfg.Identity.StateDir = "/tmp/state"
	cfg.WSS.TaskFrameBytes = 1 << 30
	if err := cfg.Validate(); err == nil {
		t.Fatal("oversized task frame must be rejected")
	}
}

func TestNormalizeJudgeModes(t *testing.T) {
	modes, err := normalizeJudgeModes([]string{"default", "SPJ", "interactive", "default"})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if strings.Join(modes, ",") != "default,spj,interactive" {
		t.Fatalf("modes %v", modes)
	}
	if _, err := normalizeJudgeModes([]string{"bogus"}); err == nil {
		t.Fatal("unknown mode must be rejected")
	}
}

func TestExampleConfigsLoad(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, name := range []string{
		filepath.Join(root, "config.example.yaml"),
		filepath.Join(root, "deploy", "config.formal.example.yaml"),
		filepath.Join(root, "deploy", "config.temp.example.yaml"),
	} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("example config %s missing: %v", name, err)
		}
		cfg, err := Load(name)
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		if cfg.HnieOJ.WSSURL == "" {
			t.Fatalf("%s: wss url not resolved", name)
		}
	}
}
