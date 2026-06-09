package config

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

type Config struct {
	Node      NodeConfig      `yaml:"node"`
	HnieOJ    HnieOJConfig    `yaml:"hnieoj"`
	RabbitMQ  RabbitMQConfig  `yaml:"rabbitmq"`
	Testdata  TestdataConfig  `yaml:"testdata"`
	GoJudge   GoJudgeConfig   `yaml:"gojudge"`
	Reporter  ReporterConfig  `yaml:"reporter"`
	Heartbeat HeartbeatConfig `yaml:"heartbeat"`
	Remote    RemoteConfig    `yaml:"remoteConfig"`
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
	FormalToken    FormalToken   `yaml:"formalToken"`
	TempToken      TempToken     `yaml:"tempToken"`
}

type FormalToken struct {
	EncryptedToken  string        `yaml:"encryptedToken"`
	PrivateKeyPath  string        `yaml:"privateKeyPath"`
	CipherAlgorithm string        `yaml:"cipherAlgorithm"`
	Nacos           NacosConfig   `yaml:"nacos"`
	RefreshInterval time.Duration `yaml:"refreshInterval"`
}

type NacosConfig struct {
	ServerAddr string `yaml:"serverAddr"`
	Namespace  string `yaml:"namespace"`
	Group      string `yaml:"group"`
	DataID     string `yaml:"dataId"`
}

type TempToken struct {
	AuthCode           string `yaml:"authCode"`
	JWT                string `yaml:"jwt"`
	TokenType          string `yaml:"tokenType"`
	NodeID             string `yaml:"nodeId"`
	TokenID            string `yaml:"tokenId"`
	ExpireTime         string `yaml:"expireTime"`
	InstanceID         string `yaml:"instanceId"`
	InstanceSecretPath string `yaml:"instanceSecretPath"`
	FingerprintHash    string `yaml:"fingerprintHash"`
	ProofType          string `yaml:"proofType"`
}

type RabbitMQConfig struct {
	Host                 string        `yaml:"host"`
	Port                 int           `yaml:"port"`
	Username             string        `yaml:"username"`
	Password             string        `yaml:"password"`
	VirtualHost          string        `yaml:"virtualHost"`
	Exchange             string        `yaml:"exchange"`
	Queue                string        `yaml:"queue"`
	RoutingKey           string        `yaml:"routingKey"`
	DeadLetterExchange   string        `yaml:"deadLetterExchange"`
	DeadLetterQueue      string        `yaml:"deadLetterQueue"`
	DeadLetterRoutingKey string        `yaml:"deadLetterRoutingKey"`
	Prefetch             int           `yaml:"prefetch"`
	MaxRetries           int           `yaml:"maxRetries"`
	RetryBackoff         time.Duration `yaml:"retryBackoff"`
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
	Mode     string `yaml:"mode"`
	Endpoint string `yaml:"endpoint"`
}

type HeartbeatConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Endpoint string        `yaml:"endpoint"`
	Interval time.Duration `yaml:"interval"`
}

type RemoteConfig struct {
	Enabled bool        `yaml:"enabled"`
	Nacos   NacosConfig `yaml:"nacos"`
}

func LoadFromArgs() (*Config, string, error) {
	var configPath string
	var fixturePath string
	flag.StringVar(&configPath, "config", "config.example.yaml", "path to HnieOJ judge node config")
	flag.StringVar(&fixturePath, "fixture", "", "run one local task fixture instead of consuming RabbitMQ")
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
	if err := applyRemoteConfig(context.Background(), cfg); err != nil {
		return nil, err
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
			FormalToken: FormalToken{
				CipherAlgorithm: "RSA/ECB/OAEPWithSHA-256AndMGF1Padding",
				PrivateKeyPath:  "/etc/hnieoj/judge-security/judge_formal_private.pem",
				RefreshInterval: 30 * time.Second,
				Nacos: NacosConfig{
					ServerAddr: "http://127.0.0.1:8848",
					Namespace:  "dev",
					Group:      "HNIEOJ_SECRET_GROUP",
					DataID:     "hnieoj-judge-formal-token.yaml",
				},
			},
		},
		RabbitMQ: RabbitMQConfig{
			Host:                 "127.0.0.1",
			Port:                 5672,
			Username:             "guest",
			Password:             "guest",
			VirtualHost:          "/",
			Exchange:             "hnieoj.judge.exchange",
			Queue:                "hnieoj.judge.task",
			RoutingKey:           "judge.submission.created",
			DeadLetterExchange:   "hnieoj.judge.dlx",
			DeadLetterQueue:      "hnieoj.judge.task.dlq",
			DeadLetterRoutingKey: "judge.submission.created.dlq",
			Prefetch:             1,
			MaxRetries:           3,
			RetryBackoff:         10 * time.Second,
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
			Mode:     "http",
			Endpoint: "/judge/submissions/{submissionId}/events",
		},
		Heartbeat: HeartbeatConfig{
			Enabled:  false,
			Endpoint: "/judge/nodes/heartbeat",
			Interval: 30 * time.Second,
		},
		Remote: RemoteConfig{
			Enabled: false,
			Nacos: NacosConfig{
				ServerAddr: "http://127.0.0.1:8848",
				Namespace:  "dev",
				Group:      "HNIEOJ_JUDGE_GROUP",
				DataID:     "hnieoj-judge-node.yaml",
			},
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
	if c.RabbitMQ.Prefetch <= 0 {
		c.RabbitMQ.Prefetch = c.Node.MaxConcurrency
	}
	if c.RabbitMQ.MaxRetries < 0 {
		c.RabbitMQ.MaxRetries = 0
	}
	if c.RabbitMQ.RetryBackoff <= 0 {
		c.RabbitMQ.RetryBackoff = 10 * time.Second
	}
	c.HnieOJ.TempToken.ProofType = strings.TrimSpace(c.HnieOJ.TempToken.ProofType)
	if c.HnieOJ.TempToken.ProofType == "" {
		c.HnieOJ.TempToken.ProofType = "hmac-sha256"
	}
	if c.HnieOJ.TempToken.ProofType != "hmac-sha256" {
		return fmt.Errorf("unsupported hnieoj.tempToken.proofType %q", c.HnieOJ.TempToken.ProofType)
	}
	return nil
}

func applyEnv(c *Config) {
	setString(&c.Node.Name, "HNIEOJ_NODE_NAME")
	setString(&c.Node.Type, "HNIEOJ_NODE_TYPE")
	setInt(&c.Node.MaxConcurrency, "HNIEOJ_NODE_MAX_CONCURRENCY")
	setStringSlice(&c.Node.SupportedJudgeModes, "HNIEOJ_NODE_SUPPORTED_JUDGE_MODES")
	setString(&c.HnieOJ.BaseURL, "HNIEOJ_BASE_URL")
	setDuration(&c.HnieOJ.RequestTimeout, "HNIEOJ_REQUEST_TIMEOUT")
	setString(&c.HnieOJ.FormalToken.EncryptedToken, "HNIEOJ_FORMAL_ENCRYPTED_TOKEN")
	setString(&c.HnieOJ.FormalToken.PrivateKeyPath, "HNIEOJ_FORMAL_PRIVATE_KEY_PATH")
	setDuration(&c.HnieOJ.FormalToken.RefreshInterval, "HNIEOJ_FORMAL_TOKEN_REFRESH_INTERVAL")
	setString(&c.HnieOJ.FormalToken.Nacos.ServerAddr, "HNIEOJ_NACOS_SERVER_ADDR")
	setString(&c.HnieOJ.FormalToken.Nacos.Namespace, "HNIEOJ_NACOS_NAMESPACE")
	setString(&c.HnieOJ.FormalToken.Nacos.Group, "HNIEOJ_FORMAL_TOKEN_NACOS_GROUP")
	setString(&c.HnieOJ.FormalToken.Nacos.DataID, "HNIEOJ_FORMAL_TOKEN_NACOS_DATA_ID")
	setString(&c.HnieOJ.TempToken.AuthCode, "HNIEOJ_TEMP_AUTH_CODE")
	setString(&c.HnieOJ.TempToken.JWT, "HNIEOJ_TEMP_JWT")
	setString(&c.HnieOJ.TempToken.TokenType, "HNIEOJ_TEMP_TOKEN_TYPE")
	setString(&c.HnieOJ.TempToken.NodeID, "HNIEOJ_TEMP_NODE_ID")
	setString(&c.HnieOJ.TempToken.TokenID, "HNIEOJ_TEMP_TOKEN_ID")
	setString(&c.HnieOJ.TempToken.ExpireTime, "HNIEOJ_TEMP_EXPIRE_TIME")
	setString(&c.HnieOJ.TempToken.InstanceID, "HNIEOJ_TEMP_INSTANCE_ID")
	setString(&c.HnieOJ.TempToken.InstanceSecretPath, "HNIEOJ_TEMP_INSTANCE_SECRET_PATH")
	setString(&c.HnieOJ.TempToken.FingerprintHash, "HNIEOJ_TEMP_FINGERPRINT_HASH")
	setString(&c.HnieOJ.TempToken.ProofType, "HNIEOJ_TEMP_PROOF_TYPE")
	setString(&c.RabbitMQ.Host, "HNIEOJ_RABBITMQ_HOST")
	setInt(&c.RabbitMQ.Port, "HNIEOJ_RABBITMQ_PORT")
	setString(&c.RabbitMQ.Username, "HNIEOJ_RABBITMQ_USERNAME")
	setString(&c.RabbitMQ.Password, "HNIEOJ_RABBITMQ_PASSWORD")
	setString(&c.RabbitMQ.VirtualHost, "HNIEOJ_RABBITMQ_VHOST")
	setString(&c.RabbitMQ.Exchange, "HNIEOJ_RABBITMQ_EXCHANGE")
	setString(&c.RabbitMQ.Queue, "HNIEOJ_RABBITMQ_QUEUE")
	setString(&c.RabbitMQ.RoutingKey, "HNIEOJ_RABBITMQ_ROUTING_KEY")
	setString(&c.RabbitMQ.DeadLetterExchange, "HNIEOJ_RABBITMQ_DLX")
	setString(&c.RabbitMQ.DeadLetterQueue, "HNIEOJ_RABBITMQ_DLQ")
	setString(&c.RabbitMQ.DeadLetterRoutingKey, "HNIEOJ_RABBITMQ_DLX_ROUTING_KEY")
	setInt(&c.RabbitMQ.Prefetch, "HNIEOJ_RABBITMQ_PREFETCH")
	setInt(&c.RabbitMQ.MaxRetries, "HNIEOJ_RABBITMQ_MAX_RETRIES")
	setDuration(&c.RabbitMQ.RetryBackoff, "HNIEOJ_RABBITMQ_RETRY_BACKOFF")
	setString(&c.Testdata.CacheRoot, "HNIEOJ_TESTDATA_CACHE_ROOT")
	setInt64(&c.Testdata.MaxCacheBytes, "HNIEOJ_TESTDATA_MAX_CACHE_BYTES")
	setDuration(&c.Testdata.MaxUnusedDuration, "HNIEOJ_TESTDATA_MAX_UNUSED_DURATION")
	setDuration(&c.Testdata.CleanupInterval, "HNIEOJ_TESTDATA_CLEANUP_INTERVAL")
	setDuration(&c.Testdata.StatsInterval, "HNIEOJ_TESTDATA_STATS_INTERVAL")
	setString(&c.GoJudge.Endpoint, "HNIEOJ_GOJUDGE_ENDPOINT")
	setString(&c.GoJudge.AuthToken, "HNIEOJ_GOJUDGE_AUTH_TOKEN")
	setString(&c.Reporter.Mode, "HNIEOJ_REPORTER_MODE")
	setString(&c.Reporter.Endpoint, "HNIEOJ_REPORTER_ENDPOINT")
	setBool(&c.Remote.Enabled, "HNIEOJ_REMOTE_CONFIG_ENABLED")
	setString(&c.Remote.Nacos.ServerAddr, "HNIEOJ_REMOTE_CONFIG_NACOS_SERVER_ADDR")
	setString(&c.Remote.Nacos.Namespace, "HNIEOJ_REMOTE_CONFIG_NACOS_NAMESPACE")
	setString(&c.Remote.Nacos.Group, "HNIEOJ_REMOTE_CONFIG_NACOS_GROUP")
	setString(&c.Remote.Nacos.DataID, "HNIEOJ_REMOTE_CONFIG_NACOS_DATA_ID")
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

type remoteConfigOverlay struct {
	Node struct {
		MaxConcurrency      *int     `yaml:"maxConcurrency"`
		SupportedJudgeModes []string `yaml:"supportedJudgeModes"`
	} `yaml:"node"`
	RabbitMQ struct {
		Prefetch     *int           `yaml:"prefetch"`
		MaxRetries   *int           `yaml:"maxRetries"`
		RetryBackoff *time.Duration `yaml:"retryBackoff"`
	} `yaml:"rabbitmq"`
	Testdata struct {
		MaxCacheBytes     *int64         `yaml:"maxCacheBytes"`
		MaxUnusedDuration *time.Duration `yaml:"maxUnusedDuration"`
		CleanupInterval   *time.Duration `yaml:"cleanupInterval"`
		StatsInterval     *time.Duration `yaml:"statsInterval"`
	} `yaml:"testdata"`
	Heartbeat struct {
		Enabled  *bool          `yaml:"enabled"`
		Endpoint *string        `yaml:"endpoint"`
		Interval *time.Duration `yaml:"interval"`
	} `yaml:"heartbeat"`
}

func applyRemoteConfig(ctx context.Context, cfg *Config) error {
	if !cfg.Remote.Enabled {
		return nil
	}
	if cfg.Remote.Nacos.ServerAddr == "" || cfg.Remote.Nacos.Group == "" || cfg.Remote.Nacos.DataID == "" {
		return errors.New("remote config nacos settings are required")
	}
	body, err := fetchNacosConfig(ctx, cfg.Remote.Nacos)
	if err != nil {
		return err
	}
	var overlay remoteConfigOverlay
	if err := yaml.Unmarshal(body, &overlay); err != nil {
		return err
	}
	mergeRemoteConfig(cfg, overlay)
	return nil
}

func fetchNacosConfig(ctx context.Context, nacos NacosConfig) ([]byte, error) {
	baseURL := strings.TrimRight(nacos.ServerAddr, "/")
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "http://" + baseURL
	}
	values := url.Values{}
	values.Set("dataId", nacos.DataID)
	values.Set("group", nacos.Group)
	if nacos.Namespace != "" {
		values.Set("tenant", nacos.Namespace)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/nacos/v1/cs/configs?"+values.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch remote config from nacos failed with status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func mergeRemoteConfig(cfg *Config, overlay remoteConfigOverlay) {
	if overlay.Node.MaxConcurrency != nil {
		cfg.Node.MaxConcurrency = *overlay.Node.MaxConcurrency
	}
	if overlay.Node.SupportedJudgeModes != nil {
		cfg.Node.SupportedJudgeModes = overlay.Node.SupportedJudgeModes
	}
	if overlay.RabbitMQ.Prefetch != nil {
		cfg.RabbitMQ.Prefetch = *overlay.RabbitMQ.Prefetch
	}
	if overlay.RabbitMQ.MaxRetries != nil {
		cfg.RabbitMQ.MaxRetries = *overlay.RabbitMQ.MaxRetries
	}
	if overlay.RabbitMQ.RetryBackoff != nil {
		cfg.RabbitMQ.RetryBackoff = *overlay.RabbitMQ.RetryBackoff
	}
	if overlay.Testdata.MaxCacheBytes != nil {
		cfg.Testdata.MaxCacheBytes = *overlay.Testdata.MaxCacheBytes
	}
	if overlay.Testdata.MaxUnusedDuration != nil {
		cfg.Testdata.MaxUnusedDuration = *overlay.Testdata.MaxUnusedDuration
	}
	if overlay.Testdata.CleanupInterval != nil {
		cfg.Testdata.CleanupInterval = *overlay.Testdata.CleanupInterval
	}
	if overlay.Testdata.StatsInterval != nil {
		cfg.Testdata.StatsInterval = *overlay.Testdata.StatsInterval
	}
	if overlay.Heartbeat.Enabled != nil {
		cfg.Heartbeat.Enabled = *overlay.Heartbeat.Enabled
	}
	if overlay.Heartbeat.Endpoint != nil {
		cfg.Heartbeat.Endpoint = *overlay.Heartbeat.Endpoint
	}
	if overlay.Heartbeat.Interval != nil {
		cfg.Heartbeat.Interval = *overlay.Heartbeat.Interval
	}
}
