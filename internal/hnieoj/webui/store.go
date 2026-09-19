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
	"github.com/criyle/go-judge/internal/hnieoj/securefile"
	"github.com/goccy/go-yaml"
	"golang.org/x/crypto/argon2"
)

const (
	ConfigFileName    = "config.yaml"
	AdminFileName     = "admin.json"
	BootstrapFileName = "bootstrap_token"
	SecurityDirName   = "security"
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

// BootstrapPath 是一次性入网 bootstrap 明文的默认路径（0600，注册成功后删除）。
func (s *Store) BootstrapPath() string {
	return filepath.Join(s.securityDir(), BootstrapFileName)
}

// Dir 返回 WebUI 状态目录。
func (s *Store) Dir() string {
	return s.dir
}

// IdentityPath 返回本地长期身份文件的默认路径。
func (s *Store) IdentityPath() string {
	return filepath.Join(s.dir, "identity.json")
}

// BootstrapConfigured 表示本地是否还有未消费的 bootstrap 明文。
func (s *Store) BootstrapConfigured() bool {
	return !emptyFile(s.BootstrapPath())
}

// WriteBootstrapToken 以 0600 原子写入 bootstrap 明文，绝不写入 config.yaml 或日志。
func (s *Store) WriteBootstrapToken(token string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("bootstrap token is required")
	}
	if err := s.Ensure(); err != nil {
		return err
	}
	return securefile.WriteFileAtomic(s.BootstrapPath(), []byte(strings.TrimSpace(token)), 0o600)
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
