package webui

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/goccy/go-yaml"
	"golang.org/x/crypto/argon2"
)

const (
	ConfigFileName         = "config.yaml"
	AdminFileName          = "admin.json"
	SecurityDirName        = "security"
	FormalPrivateKeyName   = "judge_formal_private.pem"
	TempInstanceIDName     = "temp_node_instance_id"
	TempInstanceSecretName = "temp_node_instance_secret"
)

type Store struct {
	dir string
}

type AdminRecord struct {
	Salt         string `json:"salt"`
	PasswordHash string `json:"passwordHash"`
}

func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

func (s *Store) Ensure() error {
	if err := os.MkdirAll(s.securityDir(), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return nil
}

func (s *Store) ConfigPath() string {
	return filepath.Join(s.dir, ConfigFileName)
}

func (s *Store) LoadConfig() (*config.Config, bool, error) {
	path := s.ConfigPath()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	cfg := config.Default()
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, false, err
	}
	return cfg, true, nil
}

func (s *Store) SaveConfig(cfg config.Config) error {
	if err := s.Ensure(); err != nil {
		return err
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp := s.ConfigPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.ConfigPath())
}

func (s *Store) AdminInitialized() bool {
	_, err := os.Stat(filepath.Join(s.dir, AdminFileName))
	return err == nil
}

func (s *Store) SaveAdminPassword(password string) error {
	if strings.TrimSpace(password) == "" {
		return errors.New("password is required")
	}
	if err := s.Ensure(); err != nil {
		return err
	}
	salt := randomBytes(16)
	hash := argon2.IDKey([]byte(password), salt, 1, 64*1024, 4, 32)
	record := AdminRecord{
		Salt:         base64.RawStdEncoding.EncodeToString(salt),
		PasswordHash: base64.RawStdEncoding.EncodeToString(hash),
	}
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, AdminFileName), b, 0o600)
}

func (s *Store) VerifyPassword(password string) bool {
	b, err := os.ReadFile(filepath.Join(s.dir, AdminFileName))
	if err != nil {
		return false
	}
	var record AdminRecord
	if err := json.Unmarshal(b, &record); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(record.Salt)
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(record.PasswordHash)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, 1, 64*1024, 4, 32)
	if len(got) != len(want) {
		return false
	}
	var diff byte
	for i := range got {
		diff |= got[i] ^ want[i]
	}
	return diff == 0
}

func (s *Store) SaveFormalPrivateKey(pem string) (string, error) {
	if !strings.Contains(pem, "PRIVATE KEY") {
		return "", errors.New("invalid private key pem")
	}
	if err := s.Ensure(); err != nil {
		return "", err
	}
	path := filepath.Join(s.securityDir(), FormalPrivateKeyName)
	if err := os.WriteFile(path, []byte(pem), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) EnsureTempIdentity() (string, string, error) {
	if err := s.Ensure(); err != nil {
		return "", "", err
	}
	idPath := filepath.Join(s.securityDir(), TempInstanceIDName)
	secretPath := filepath.Join(s.securityDir(), TempInstanceSecretName)
	if emptyFile(idPath) {
		if err := os.WriteFile(idPath, []byte(randomToken(24)), 0o600); err != nil {
			return "", "", err
		}
	}
	if emptyFile(secretPath) {
		if err := os.WriteFile(secretPath, []byte(randomToken(32)), 0o600); err != nil {
			return "", "", err
		}
	}
	id, err := os.ReadFile(idPath)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(id)), secretPath, nil
}

func (s *Store) securityDir() string {
	return filepath.Join(s.dir, SecurityDirName)
}

func emptyFile(path string) bool {
	stat, err := os.Stat(path)
	return err != nil || stat.Size() == 0
}

func randomToken(size int) string {
	return base64.RawURLEncoding.EncodeToString(randomBytes(size))
}

func randomBytes(size int) []byte {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
