package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
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
}

type NodeConfig struct {
	Name           string `yaml:"name"`
	Type           string `yaml:"type"`
	MaxConcurrency int    `yaml:"maxConcurrency"`
}

type HnieOJConfig struct {
	BaseURL        string        `yaml:"baseUrl"`
	RequestTimeout time.Duration `yaml:"requestTimeout"`
	FormalToken    FormalToken   `yaml:"formalToken"`
	TempToken      TempToken     `yaml:"tempToken"`
}

type FormalToken struct {
	EncryptedToken  string `yaml:"encryptedToken"`
	PrivateKeyPath  string `yaml:"privateKeyPath"`
	CipherAlgorithm string `yaml:"cipherAlgorithm"`
}

type TempToken struct {
	AuthCode string `yaml:"authCode"`
}

type RabbitMQConfig struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	VirtualHost string `yaml:"virtualHost"`
	Exchange    string `yaml:"exchange"`
	Queue       string `yaml:"queue"`
	RoutingKey  string `yaml:"routingKey"`
	Prefetch    int    `yaml:"prefetch"`
}

type TestdataConfig struct {
	CacheRoot string `yaml:"cacheRoot"`
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
	return cfg, cfg.Validate()
}

func defaultConfig() *Config {
	return &Config{
		Node: NodeConfig{
			Name:           "judge-node-01",
			Type:           "formal",
			MaxConcurrency: 1,
		},
		HnieOJ: HnieOJConfig{
			RequestTimeout: 30 * time.Second,
			FormalToken: FormalToken{
				CipherAlgorithm: "RSA/ECB/OAEPWithSHA-256AndMGF1Padding",
			},
		},
		RabbitMQ: RabbitMQConfig{
			Host:        "127.0.0.1",
			Port:        5672,
			Username:    "guest",
			Password:    "guest",
			VirtualHost: "/",
			Exchange:    "hnieoj.judge.exchange",
			Queue:       "hnieoj.judge.task",
			RoutingKey:  "judge.submission.created",
			Prefetch:    1,
		},
		Testdata: TestdataConfig{
			CacheRoot: "/data/oj/judge-cache",
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
	if c.HnieOJ.BaseURL == "" {
		return errors.New("hnieoj.baseUrl is required")
	}
	if c.GoJudge.Endpoint == "" {
		return errors.New("gojudge.endpoint is required")
	}
	if c.Testdata.CacheRoot == "" {
		return errors.New("testdata.cacheRoot is required")
	}
	if c.RabbitMQ.Prefetch <= 0 {
		c.RabbitMQ.Prefetch = c.Node.MaxConcurrency
	}
	return nil
}

func applyEnv(c *Config) {
	setString(&c.Node.Name, "HNIEOJ_NODE_NAME")
	setString(&c.Node.Type, "HNIEOJ_NODE_TYPE")
	setInt(&c.Node.MaxConcurrency, "HNIEOJ_NODE_MAX_CONCURRENCY")
	setString(&c.HnieOJ.BaseURL, "HNIEOJ_BASE_URL")
	setDuration(&c.HnieOJ.RequestTimeout, "HNIEOJ_REQUEST_TIMEOUT")
	setString(&c.HnieOJ.FormalToken.EncryptedToken, "HNIEOJ_FORMAL_ENCRYPTED_TOKEN")
	setString(&c.HnieOJ.FormalToken.PrivateKeyPath, "HNIEOJ_FORMAL_PRIVATE_KEY_PATH")
	setString(&c.HnieOJ.TempToken.AuthCode, "HNIEOJ_TEMP_AUTH_CODE")
	setString(&c.RabbitMQ.Host, "HNIEOJ_RABBITMQ_HOST")
	setInt(&c.RabbitMQ.Port, "HNIEOJ_RABBITMQ_PORT")
	setString(&c.RabbitMQ.Username, "HNIEOJ_RABBITMQ_USERNAME")
	setString(&c.RabbitMQ.Password, "HNIEOJ_RABBITMQ_PASSWORD")
	setString(&c.RabbitMQ.VirtualHost, "HNIEOJ_RABBITMQ_VHOST")
	setString(&c.RabbitMQ.Exchange, "HNIEOJ_RABBITMQ_EXCHANGE")
	setString(&c.RabbitMQ.Queue, "HNIEOJ_RABBITMQ_QUEUE")
	setString(&c.RabbitMQ.RoutingKey, "HNIEOJ_RABBITMQ_ROUTING_KEY")
	setInt(&c.RabbitMQ.Prefetch, "HNIEOJ_RABBITMQ_PREFETCH")
	setString(&c.Testdata.CacheRoot, "HNIEOJ_TESTDATA_CACHE_ROOT")
	setString(&c.GoJudge.Endpoint, "HNIEOJ_GOJUDGE_ENDPOINT")
	setString(&c.GoJudge.AuthToken, "HNIEOJ_GOJUDGE_AUTH_TOKEN")
	setString(&c.Reporter.Mode, "HNIEOJ_REPORTER_MODE")
	setString(&c.Reporter.Endpoint, "HNIEOJ_REPORTER_ENDPOINT")
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

func setDuration(dst *time.Duration, key string) {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			*dst = d
		}
	}
}
