package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Config 是判题节点 agent 的完整运行配置。
// 身份与任务通道：本地 Ed25519 身份 + 一次性 Bootstrap + WSS；
// HTTPS 只用于入网、签名测试数据下载与密钥轮换。不再有 RabbitMQ/Nacos/shared token。
type Config struct {
	Node      NodeConfig      `yaml:"node"`
	HnieOJ    HnieOJConfig    `yaml:"hnieoj"`
	Identity  IdentityConfig  `yaml:"identity"`
	Bootstrap BootstrapConfig `yaml:"bootstrap"`
	WSS       WSSConfig       `yaml:"wss"`
	Rotation  RotationConfig  `yaml:"rotation"`
	Testdata  TestdataConfig  `yaml:"testdata"`
	GoJudge   GoJudgeConfig   `yaml:"gojudge"`
	Worker    WorkerConfig    `yaml:"worker"`
}

type NodeConfig struct {
	Name                string   `yaml:"name"`
	Type                string   `yaml:"type"`
	MaxConcurrency      int      `yaml:"maxConcurrency"`
	SupportedJudgeModes []string `yaml:"supportedJudgeModes"`
}

type HnieOJConfig struct {
	BaseURL string `yaml:"baseUrl"`
	// WSSURL 为任务通道地址；留空时由 baseUrl 推导 wss://host/ws/judge/node。
	WSSURL string `yaml:"wssUrl"`
	// Audience 应与服务端配置一致；AUTH_OK 的 audience 由 challenge 下发并为准。
	Audience       string        `yaml:"audience"`
	RequestTimeout time.Duration `yaml:"requestTimeout"`
}

// IdentityConfig 指定本地长期身份文件。私钥只在此文件，永不外发。
type IdentityConfig struct {
	File string `yaml:"file"`
	// StateDir 保存结果队列等运行期安全状态。
	StateDir string `yaml:"stateDir"`
}

// BootstrapConfig 是一次性入网凭证来源；注册成功后文件被删除。
type BootstrapConfig struct {
	TokenFile string `yaml:"tokenFile"`
	Token     string `yaml:"token"`
}

// WSSConfig 控制任务通道边界；所有值都有正的上界。
type WSSConfig struct {
	ControlFrameBytes    int           `yaml:"controlFrameBytes"`
	TaskFrameBytes       int           `yaml:"taskFrameBytes"`
	AuthDeadline         time.Duration `yaml:"authDeadline"`
	RequestTimeout       time.Duration `yaml:"requestTimeout"`
	ConnectMinBackoff    time.Duration `yaml:"connectMinBackoff"`
	ConnectMaxBackoff    time.Duration `yaml:"connectMaxBackoff"`
	HeartbeatInterval    time.Duration `yaml:"heartbeatInterval"`
	WriteTimeout         time.Duration `yaml:"writeTimeout"`
	ResultQueueDir       string        `yaml:"resultQueueDir"`
	ResultMaxRecords     int           `yaml:"resultMaxRecords"`
	ResultMaxBytes       int64         `yaml:"resultMaxBytes"`
	ResultMaxRecordBytes int64         `yaml:"resultMaxRecordBytes"`
	ResultTTL            time.Duration `yaml:"resultTtl"`
	ResultRetryBackoff   time.Duration `yaml:"resultRetryBackoff"`
}

// RotationConfig 控制自动密钥轮换；默认 30 天，测试可注入短周期。
type RotationConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Interval       time.Duration `yaml:"interval"`
	Grace          time.Duration `yaml:"grace"`
	ConfirmTimeout time.Duration `yaml:"confirmTimeout"`
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

// WorkerConfig 控制有界槽位执行与优雅排空。
type WorkerConfig struct {
	EmptyMinBackoff time.Duration `yaml:"emptyMinBackoff"`
	EmptyMaxBackoff time.Duration `yaml:"emptyMaxBackoff"`
	DrainTimeout    time.Duration `yaml:"drainTimeout"`
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

// Default 返回带默认值的配置，供 WebUI 首次进入配置页时使用。
func Default() *Config {
	return defaultConfig()
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
			BaseURL:        "https://oj.example.com",
			RequestTimeout: 30 * time.Second,
		},
		Identity: IdentityConfig{
			File:     "/var/lib/hnieoj-judge-node/identity.json",
			StateDir: "/var/lib/hnieoj-judge-node",
		},
		WSS: WSSConfig{
			ControlFrameBytes:    64 * 1024,
			TaskFrameBytes:       4 * 1024 * 1024,
			AuthDeadline:         10 * time.Second,
			RequestTimeout:       30 * time.Second,
			ConnectMinBackoff:    500 * time.Millisecond,
			ConnectMaxBackoff:    30 * time.Second,
			HeartbeatInterval:    30 * time.Second,
			WriteTimeout:         10 * time.Second,
			ResultMaxRecords:     256,
			ResultMaxBytes:       64 * 1024 * 1024,
			ResultMaxRecordBytes: 4 * 1024 * 1024,
			ResultTTL:            72 * time.Hour,
			ResultRetryBackoff:   2 * time.Second,
		},
		Rotation: RotationConfig{
			Enabled:        true,
			Interval:       30 * 24 * time.Hour,
			Grace:          5 * time.Minute,
			ConfirmTimeout: 30 * time.Second,
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
	if strings.TrimSpace(c.HnieOJ.WSSURL) == "" {
		derived, err := DeriveWSSURL(c.HnieOJ.BaseURL)
		if err != nil {
			return err
		}
		c.HnieOJ.WSSURL = derived
	}
	if err := ValidateWSSURL(c.HnieOJ.WSSURL); err != nil {
		return err
	}
	if c.HnieOJ.RequestTimeout <= 0 {
		c.HnieOJ.RequestTimeout = 30 * time.Second
	}
	if strings.TrimSpace(c.Identity.File) == "" {
		return errors.New("identity.file is required")
	}
	if strings.TrimSpace(c.Identity.StateDir) == "" {
		return errors.New("identity.stateDir is required")
	}
	if c.WSS.ControlFrameBytes <= 0 {
		c.WSS.ControlFrameBytes = 64 * 1024
	}
	if c.WSS.ControlFrameBytes > 64*1024*1024 {
		return errors.New("wss.controlFrameBytes exceeds hard limit")
	}
	if c.WSS.TaskFrameBytes <= 0 {
		c.WSS.TaskFrameBytes = 4 * 1024 * 1024
	}
	if c.WSS.TaskFrameBytes > 64*1024*1024 {
		return errors.New("wss.taskFrameBytes exceeds hard limit")
	}
	if c.WSS.AuthDeadline <= 0 {
		c.WSS.AuthDeadline = 10 * time.Second
	}
	if c.WSS.RequestTimeout <= 0 {
		c.WSS.RequestTimeout = 30 * time.Second
	}
	if c.WSS.ConnectMinBackoff <= 0 {
		c.WSS.ConnectMinBackoff = 500 * time.Millisecond
	}
	if c.WSS.ConnectMaxBackoff < c.WSS.ConnectMinBackoff {
		c.WSS.ConnectMaxBackoff = 30 * time.Second
	}
	if c.WSS.HeartbeatInterval <= 0 {
		c.WSS.HeartbeatInterval = 30 * time.Second
	}
	if c.WSS.WriteTimeout <= 0 {
		c.WSS.WriteTimeout = 10 * time.Second
	}
	if c.WSS.ResultMaxRecords <= 0 {
		c.WSS.ResultMaxRecords = 256
	}
	if c.WSS.ResultMaxBytes <= 0 {
		c.WSS.ResultMaxBytes = 64 * 1024 * 1024
	}
	if c.WSS.ResultMaxRecordBytes <= 0 {
		c.WSS.ResultMaxRecordBytes = 4 * 1024 * 1024
	}
	if c.WSS.ResultTTL < 0 {
		return errors.New("wss.resultTtl must not be negative")
	}
	if c.WSS.ResultRetryBackoff <= 0 {
		c.WSS.ResultRetryBackoff = 2 * time.Second
	}
	if c.WSS.ResultQueueDir == "" {
		c.WSS.ResultQueueDir = c.Identity.StateDir + "/results"
	}
	if c.Rotation.Interval <= 0 {
		c.Rotation.Interval = 30 * 24 * time.Hour
	}
	if c.Rotation.Grace <= 0 {
		c.Rotation.Grace = 5 * time.Minute
	}
	if c.Rotation.ConfirmTimeout <= 0 {
		c.Rotation.ConfirmTimeout = 30 * time.Second
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

// DeriveWSSURL 由 baseUrl 推导 wss 任务通道地址。
func DeriveWSSURL(baseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}
	scheme := "wss"
	if strings.EqualFold(u.Scheme, "http") {
		scheme = "ws"
	}
	return scheme + "://" + u.Host + "/ws/judge/node", nil
}

// ValidateBackendURL 要求远程后端必须使用 HTTPS；只有显式回环地址才允许明文 HTTP 用于本地开发。
// 不允许关闭证书校验，也不提供任何 skip-cert 开关。
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

// ValidateWSSURL 要求远程必须 wss；ws 只允许显式 loopback 开发地址。
func ValidateWSSURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("hnieoj.wssUrl is invalid: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("hnieoj.wssUrl must be an absolute URL, got %q", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "wss", "https":
		return nil
	case "ws", "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("hnieoj.wssUrl must use wss for non-loopback hosts, got %q", raw)
	default:
		return fmt.Errorf("hnieoj.wssUrl must use ws or wss, got %q", raw)
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
	setString(&c.HnieOJ.WSSURL, "HNIEOJ_WSS_URL")
	setString(&c.HnieOJ.Audience, "HNIEOJ_AUDIENCE")
	setDuration(&c.HnieOJ.RequestTimeout, "HNIEOJ_REQUEST_TIMEOUT")
	setString(&c.Identity.File, "HNIEOJ_IDENTITY_FILE")
	setString(&c.Identity.StateDir, "HNIEOJ_STATE_DIR")
	setString(&c.Bootstrap.TokenFile, "HNIEOJ_BOOTSTRAP_TOKEN_FILE")
	setString(&c.Bootstrap.Token, "HNIEOJ_BOOTSTRAP_TOKEN")
	setInt(&c.WSS.ControlFrameBytes, "HNIEOJ_WSS_CONTROL_FRAME_BYTES")
	setInt(&c.WSS.TaskFrameBytes, "HNIEOJ_WSS_TASK_FRAME_BYTES")
	setDuration(&c.WSS.AuthDeadline, "HNIEOJ_WSS_AUTH_DEADLINE")
	setDuration(&c.WSS.RequestTimeout, "HNIEOJ_WSS_REQUEST_TIMEOUT")
	setDuration(&c.WSS.ConnectMinBackoff, "HNIEOJ_WSS_CONNECT_MIN_BACKOFF")
	setDuration(&c.WSS.ConnectMaxBackoff, "HNIEOJ_WSS_CONNECT_MAX_BACKOFF")
	setDuration(&c.WSS.HeartbeatInterval, "HNIEOJ_WSS_HEARTBEAT_INTERVAL")
	setDuration(&c.WSS.WriteTimeout, "HNIEOJ_WSS_WRITE_TIMEOUT")
	setString(&c.WSS.ResultQueueDir, "HNIEOJ_RESULT_QUEUE_DIR")
	setInt(&c.WSS.ResultMaxRecords, "HNIEOJ_RESULT_MAX_RECORDS")
	setInt64(&c.WSS.ResultMaxBytes, "HNIEOJ_RESULT_MAX_BYTES")
	setInt64(&c.WSS.ResultMaxRecordBytes, "HNIEOJ_RESULT_MAX_RECORD_BYTES")
	setDuration(&c.WSS.ResultTTL, "HNIEOJ_RESULT_TTL")
	setDuration(&c.WSS.ResultRetryBackoff, "HNIEOJ_RESULT_RETRY_BACKOFF")
	setBool(&c.Rotation.Enabled, "HNIEOJ_ROTATION_ENABLED")
	setDuration(&c.Rotation.Interval, "HNIEOJ_ROTATION_INTERVAL")
	setDuration(&c.Rotation.Grace, "HNIEOJ_ROTATION_GRACE")
	setDuration(&c.Rotation.ConfirmTimeout, "HNIEOJ_ROTATION_CONFIRM_TIMEOUT")
	setString(&c.Testdata.CacheRoot, "HNIEOJ_TESTDATA_CACHE_ROOT")
	setInt64(&c.Testdata.MaxCacheBytes, "HNIEOJ_TESTDATA_MAX_CACHE_BYTES")
	setDuration(&c.Testdata.MaxUnusedDuration, "HNIEOJ_TESTDATA_MAX_UNUSED_DURATION")
	setDuration(&c.Testdata.CleanupInterval, "HNIEOJ_TESTDATA_CLEANUP_INTERVAL")
	setDuration(&c.Testdata.StatsInterval, "HNIEOJ_TESTDATA_STATS_INTERVAL")
	setString(&c.GoJudge.Endpoint, "HNIEOJ_GOJUDGE_ENDPOINT")
	setString(&c.GoJudge.AuthToken, "HNIEOJ_GOJUDGE_AUTH_TOKEN")
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
