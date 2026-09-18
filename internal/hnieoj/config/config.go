package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Config 是判题节点的完整运行配置。
// 节点运行期只访问后端 HTTPS 网关与本地沙箱，不依赖 RabbitMQ/Nacos/Redis。
type Config struct {
	Node      NodeConfig      `yaml:"node"`
	HnieOJ    HnieOJConfig    `yaml:"hnieoj"`
	Testdata  TestdataConfig  `yaml:"testdata"`
	GoJudge   GoJudgeConfig   `yaml:"gojudge"`
	Reporter  ReporterConfig  `yaml:"reporter"`
	Heartbeat HeartbeatConfig `yaml:"heartbeat"`
	Worker    WorkerConfig    `yaml:"worker"`
}

type NodeConfig struct {
	Name                string   `yaml:"name"`
	Type                string   `yaml:"type"`
	MaxConcurrency      int      `yaml:"maxConcurrency"`
	SupportedJudgeModes []string `yaml:"supportedJudgeModes"`
}

type HnieOJConfig struct {
	BaseURL        string        `yaml:"baseUrl"`
	RequestTimeout time.Duration `yaml:"requestTimeout"`
	Credential     Credential    `yaml:"credential"`
	Renew          RenewConfig   `yaml:"renew"`
}

// Credential 描述运维交付的逐节点运行凭证来源。
// 允许的方式：
//  1. formal：运维通过 credential.tokenFile 交付逐节点 Bearer 凭证 JSON（推荐），
//     或通过 credential.token 内联配置（不写日志）；节点续期后原子写回文件。
//  2. temp：首次用 credential.authCode 调用 /api/judge/temp-token 注册，
//     成功后原子写入 tokenFile；之后只用 /judge/nodes/token/renew 续期，不再兑换授权码。
//
// 不再支持共享 formal 主密钥、RSA 密文或 Nacos。
type Credential struct {
	// TokenFile 是运行凭证 JSON 文件路径。formal 由运维预置；temp 首次兑换后自动写入。
	// 续期成功后节点会以 0600 权限原子替换该文件，重启时优先读取最新凭证。
	TokenFile string `yaml:"tokenFile"`
	// Token 是可直接使用的逐节点 Bearer JWT。留空则只使用 tokenFile。
	Token string `yaml:"token"`
	// AuthCode 仅用于 temp 节点首次注册，不得用于运行期续期。
	AuthCode string `yaml:"authCode"`
}

// RenewConfig 控制运行凭证续期节奏。
type RenewConfig struct {
	// SafetyMargin 是相对后端签发 JWT exp 的提前续期量，避免节点时区影响。
	// 当签发 TTL 小于该余量时，节点按剩余有效期的一小部分再次续期，保证不会睡过新 exp。
	SafetyMargin time.Duration `yaml:"safetyMargin"`
	// RetryBackoff 是续期网络失败后的重试退避。
	RetryBackoff time.Duration `yaml:"retryBackoff"`
}

type TestdataConfig struct {
	CacheRoot         string        `yaml:"cacheRoot"`
	MaxCacheBytes     int64         `yaml:"maxCacheBytes"`
	MaxUnusedDuration time.Duration `yaml:"maxUnusedDuration"`
	CleanupInterval   time.Duration `yaml:"cleanupInterval"`
	StatsInterval     time.Duration `yaml:"statsInterval"`
}

type GoJudgeConfig struct {
	Endpoint  string `yaml:"endpoint"`
	AuthToken string `yaml:"authToken"`
}

type ReporterConfig struct {
	Mode         string        `yaml:"mode"`
	Endpoint     string        `yaml:"endpoint"`
	MaxRetries   int           `yaml:"maxRetries"`
	RetryBackoff time.Duration `yaml:"retryBackoff"`
}

type HeartbeatConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Endpoint string        `yaml:"endpoint"`
	Interval time.Duration `yaml:"interval"`
}

// WorkerConfig 控制有界槽位领取与优雅排空。
type WorkerConfig struct {
	EmptyMinBackoff time.Duration `yaml:"emptyMinBackoff"`
	EmptyMaxBackoff time.Duration `yaml:"emptyMaxBackoff"`
	DrainTimeout    time.Duration `yaml:"drainTimeout"`
}

func LoadFromArgs() (*Config, string, error) {
	var configPath string
	var fixturePath string
	flag.StringVar(&configPath, "config", "config.example.yaml", "path to HnieOJ judge node config")
	flag.StringVar(&fixturePath, "fixture", "", "run one local task fixture instead of polling the backend gateway (log reporter only)")
	flag.Parse()

	cfg, err := Load(configPath)
	if err != nil {
		return nil, "", err
	}
	return cfg, fixturePath, nil
}

func Load(path string) (*Config, error) {
	cfg := defaultConfig()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, err
		}
	}
	applyEnv(cfg)
	return cfg, cfg.Validate()
}

func defaultConfig() *Config {
	return &Config{
		Node: NodeConfig{
			Name:                "judge-node-01",
			Type:                "formal",
			MaxConcurrency:      1,
			SupportedJudgeModes: []string{"default"},
		},
		HnieOJ: HnieOJConfig{
			RequestTimeout: 30 * time.Second,
			Credential: Credential{
				TokenFile: "/etc/hnieoj/judge-node/credential.json",
			},
			Renew: RenewConfig{
				SafetyMargin: 30 * time.Second,
				RetryBackoff: 10 * time.Second,
			},
		},
		Testdata: TestdataConfig{
			CacheRoot:         "/data/oj/judge-cache",
			MaxCacheBytes:     20 * 1024 * 1024 * 1024,
			MaxUnusedDuration: 72 * time.Hour,
			CleanupInterval:   time.Hour,
			StatsInterval:     5 * time.Minute,
		},
		GoJudge: GoJudgeConfig{
			Endpoint: "http://127.0.0.1:5050",
		},
		Reporter: ReporterConfig{
			Mode:         "http",
			Endpoint:     "/judge/submissions/{submissionId}/events",
			MaxRetries:   3,
			RetryBackoff: 2 * time.Second,
		},
		Heartbeat: HeartbeatConfig{
			Enabled:  false,
			Endpoint: "/judge/nodes/heartbeat",
			Interval: 30 * time.Second,
		},
		Worker: WorkerConfig{
			EmptyMinBackoff: 200 * time.Millisecond,
			EmptyMaxBackoff: 5 * time.Second,
			DrainTimeout:    5 * time.Minute,
		},
	}
}

func (c *Config) Validate() error {
	if c.Node.Name == "" {
		return errors.New("node.name is required")
	}
	if c.Node.Type != "formal" && c.Node.Type != "temp" {
		return fmt.Errorf("unsupported node.type %q", c.Node.Type)
	}
	if c.Node.MaxConcurrency <= 0 {
		return errors.New("node.maxConcurrency must be positive")
	}
	modes, err := normalizeJudgeModes(c.Node.SupportedJudgeModes)
	if err != nil {
		return err
	}
	c.Node.SupportedJudgeModes = modes
	if c.HnieOJ.BaseURL == "" {
		return errors.New("hnieoj.baseUrl is required")
	}
	if err := ValidateBackendURL(c.HnieOJ.BaseURL); err != nil {
		return err
	}
	if c.HnieOJ.RequestTimeout <= 0 {
		c.HnieOJ.RequestTimeout = 30 * time.Second
	}
	if c.HnieOJ.Credential.TokenFile == "" && strings.TrimSpace(c.HnieOJ.Credential.Token) == "" &&
		(c.Node.Type != "temp" || strings.TrimSpace(c.HnieOJ.Credential.AuthCode) == "") {
		return errors.New("hnieoj.credential requires tokenFile, token or temp authCode")
	}
	if c.Node.Type == "formal" && strings.TrimSpace(c.HnieOJ.Credential.Token) == "" && c.HnieOJ.Credential.TokenFile == "" {
		return errors.New("formal node requires hnieoj.credential.tokenFile or hnieoj.credential.token")
	}
	if c.HnieOJ.Renew.SafetyMargin <= 0 {
		c.HnieOJ.Renew.SafetyMargin = 30 * time.Second
	}
	if c.HnieOJ.Renew.RetryBackoff <= 0 {
		c.HnieOJ.Renew.RetryBackoff = 10 * time.Second
	}
	if c.GoJudge.Endpoint == "" {
		return errors.New("gojudge.endpoint is required")
	}
	if c.Testdata.CacheRoot == "" {
		return errors.New("testdata.cacheRoot is required")
	}
	if c.Testdata.MaxCacheBytes < 0 {
		return errors.New("testdata.maxCacheBytes must not be negative")
	}
	if c.Testdata.MaxUnusedDuration < 0 {
		return errors.New("testdata.maxUnusedDuration must not be negative")
	}
	if c.Testdata.CleanupInterval <= 0 {
		c.Testdata.CleanupInterval = time.Hour
	}
	if c.Testdata.StatsInterval <= 0 {
		c.Testdata.StatsInterval = 5 * time.Minute
	}
	switch c.Reporter.Mode {
	case "", "http":
		c.Reporter.Mode = "http"
		if c.Reporter.Endpoint == "" {
			return errors.New("reporter.endpoint is required for http reporter")
		}
	case "log", "mock":
		c.Reporter.Mode = "log"
	default:
		return fmt.Errorf("unsupported reporter.mode %q", c.Reporter.Mode)
	}
	if c.Reporter.MaxRetries < 0 {
		c.Reporter.MaxRetries = 0
	}
	if c.Reporter.RetryBackoff <= 0 {
		c.Reporter.RetryBackoff = 2 * time.Second
	}
	if c.Heartbeat.Enabled {
		if c.Heartbeat.Endpoint == "" {
			return errors.New("heartbeat.endpoint is required when heartbeat is enabled")
		}
		if c.Heartbeat.Interval <= 0 {
			c.Heartbeat.Interval = 30 * time.Second
		}
	}
	if c.Worker.EmptyMinBackoff <= 0 {
		c.Worker.EmptyMinBackoff = 200 * time.Millisecond
	}
	if c.Worker.EmptyMaxBackoff < c.Worker.EmptyMinBackoff {
		c.Worker.EmptyMaxBackoff = 5 * time.Second
	}
	if c.Worker.DrainTimeout <= 0 {
		c.Worker.DrainTimeout = 5 * time.Minute
	}
	return nil
}

// ValidateBackendURL 要求远程后端必须使用 HTTPS；只有显式回环地址才允许明文 HTTP 用于本地开发。
// 这里不允许关闭证书校验，也不提供任何 skip-cert 开关。
func ValidateBackendURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("hnieoj.baseUrl is invalid: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("hnieoj.baseUrl must be an absolute URL, got %q", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("hnieoj.baseUrl must use https for non-loopback hosts, got %q", raw)
	default:
		return fmt.Errorf("hnieoj.baseUrl must use http or https, got %q", raw)
	}
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func applyEnv(c *Config) {
	setString(&c.Node.Name, "HNIEOJ_NODE_NAME")
	setString(&c.Node.Type, "HNIEOJ_NODE_TYPE")
	setInt(&c.Node.MaxConcurrency, "HNIEOJ_NODE_MAX_CONCURRENCY")
	setStringSlice(&c.Node.SupportedJudgeModes, "HNIEOJ_NODE_SUPPORTED_JUDGE_MODES")
	setString(&c.HnieOJ.BaseURL, "HNIEOJ_BASE_URL")
	setDuration(&c.HnieOJ.RequestTimeout, "HNIEOJ_REQUEST_TIMEOUT")
	setString(&c.HnieOJ.Credential.TokenFile, "HNIEOJ_CREDENTIAL_TOKEN_FILE")
	setString(&c.HnieOJ.Credential.Token, "HNIEOJ_CREDENTIAL_TOKEN")
	setString(&c.HnieOJ.Credential.AuthCode, "HNIEOJ_TEMP_AUTH_CODE")
	setDuration(&c.HnieOJ.Renew.SafetyMargin, "HNIEOJ_RENEW_SAFETY_MARGIN")
	setDuration(&c.HnieOJ.Renew.RetryBackoff, "HNIEOJ_RENEW_RETRY_BACKOFF")
	setString(&c.Testdata.CacheRoot, "HNIEOJ_TESTDATA_CACHE_ROOT")
	setInt64(&c.Testdata.MaxCacheBytes, "HNIEOJ_TESTDATA_MAX_CACHE_BYTES")
	setDuration(&c.Testdata.MaxUnusedDuration, "HNIEOJ_TESTDATA_MAX_UNUSED_DURATION")
	setDuration(&c.Testdata.CleanupInterval, "HNIEOJ_TESTDATA_CLEANUP_INTERVAL")
	setDuration(&c.Testdata.StatsInterval, "HNIEOJ_TESTDATA_STATS_INTERVAL")
	setString(&c.GoJudge.Endpoint, "HNIEOJ_GOJUDGE_ENDPOINT")
	setString(&c.GoJudge.AuthToken, "HNIEOJ_GOJUDGE_AUTH_TOKEN")
	setString(&c.Reporter.Mode, "HNIEOJ_REPORTER_MODE")
	setString(&c.Reporter.Endpoint, "HNIEOJ_REPORTER_ENDPOINT")
	setInt(&c.Reporter.MaxRetries, "HNIEOJ_REPORTER_MAX_RETRIES")
	setDuration(&c.Reporter.RetryBackoff, "HNIEOJ_REPORTER_RETRY_BACKOFF")
	setBool(&c.Heartbeat.Enabled, "HNIEOJ_HEARTBEAT_ENABLED")
	setString(&c.Heartbeat.Endpoint, "HNIEOJ_HEARTBEAT_ENDPOINT")
	setDuration(&c.Heartbeat.Interval, "HNIEOJ_HEARTBEAT_INTERVAL")
	setDuration(&c.Worker.EmptyMinBackoff, "HNIEOJ_WORKER_EMPTY_MIN_BACKOFF")
	setDuration(&c.Worker.EmptyMaxBackoff, "HNIEOJ_WORKER_EMPTY_MAX_BACKOFF")
	setDuration(&c.Worker.DrainTimeout, "HNIEOJ_WORKER_DRAIN_TIMEOUT")
}

func setString(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func setInt(dst *int, key string) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func setInt64(dst *int64, key string) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			*dst = n
		}
	}
}

func setBool(dst *bool, key string) {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = b
		}
	}
}

func setDuration(dst *time.Duration, key string) {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			*dst = d
		}
	}
}

func setStringSlice(dst *[]string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = splitCSV(v)
	}
}

func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func normalizeJudgeModes(modes []string) ([]string, error) {
	if len(modes) == 0 {
		return []string{"default"}, nil
	}
	allowed := map[string]struct{}{
		"default":     {},
		"spj":         {},
		"interactive": {},
	}
	seen := make(map[string]struct{}, len(modes))
	out := make([]string, 0, len(modes))
	for _, mode := range modes {
		mode = strings.ToLower(strings.TrimSpace(mode))
		if mode == "" {
			continue
		}
		if _, ok := allowed[mode]; !ok {
			return nil, fmt.Errorf("unsupported node.supportedJudgeModes value %q", mode)
		}
		if _, ok := seen[mode]; ok {
			continue
		}
		seen[mode] = struct{}{}
		out = append(out, mode)
	}
	if len(out) == 0 {
		return []string{"default"}, nil
	}
	return out, nil
}
