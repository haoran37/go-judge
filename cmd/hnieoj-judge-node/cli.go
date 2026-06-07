package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/config"
)

const (
	defaultConfigPath     = "/etc/hnieoj/go-judge/config.yaml"
	defaultComposePath    = "/etc/hnieoj/go-judge/docker-compose.yml"
	defaultSecurityDir    = "/etc/hnieoj/judge-security"
	defaultPrivateKeyName = "judge_formal_private.pem"
	defaultCacheRoot      = "/data/oj/judge-cache"
	defaultTokenCachePath = "/etc/hnieoj/go-judge/temp-token.json"
)

func handleCLI(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "init":
		return true, runInit(args[1:])
	case "auth-exchange":
		return true, runAuthExchange(args[1:])
	case "config-validate":
		return true, runConfigValidate(args[1:])
	case "doctor":
		return true, runDoctor(args[1:])
	case "help", "-h", "--help":
		printCLIUsage()
		return true, nil
	default:
		return false, nil
	}
}

func printCLIUsage() {
	fmt.Println(`Usage:
  hnieoj-judge-node init [-config /etc/hnieoj/go-judge/config.yaml] [-compose /etc/hnieoj/go-judge/docker-compose.yml]
  hnieoj-judge-node auth-exchange [-config /etc/hnieoj/go-judge/config.yaml]
  hnieoj-judge-node config-validate [-config /etc/hnieoj/go-judge/config.yaml]
  hnieoj-judge-node doctor [-config /etc/hnieoj/go-judge/config.yaml]
  hnieoj-judge-node -config /etc/hnieoj/go-judge/config.yaml`)
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file path")
	composePath := fs.String("compose", defaultComposePath, "docker compose file path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Println("HnieOJ judge node interactive initializer")
	nodeName := prompt(reader, "Node name", defaultHostname())
	nodeType := promptChoice(reader, "Node type", "formal", []string{"formal", "temp"})
	maxConcurrency := promptInt(reader, "Max concurrency", 2)
	baseURL := prompt(reader, "HnieOJ backend base URL for this node", "http://gateway:8800")
	nacosServer := prompt(reader, "Nacos server address", "http://nacos:8848")
	nacosNamespace := prompt(reader, "Nacos namespace", "dev")
	rabbitHost := prompt(reader, "RabbitMQ host", "rabbitmq")
	rabbitPort := promptInt(reader, "RabbitMQ port", 5672)
	rabbitUser := prompt(reader, "RabbitMQ username", "hnieoj_judge")
	rabbitPassword := promptRequired(reader, "RabbitMQ password")
	rabbitVhost := prompt(reader, "RabbitMQ vhost", "hnieoj")
	cacheRoot := prompt(reader, "Testdata cache directory", defaultCacheRoot)
	image := prompt(reader, "Docker image", "hnieoj/go-judge:dev")
	dockerNetwork := prompt(reader, "Existing Docker network, use private to create a new network", "hnieoj-dev_hnieoj-backend")

	securityDir := defaultSecurityDir
	privateKeyPath := filepath.Join(securityDir, defaultPrivateKeyName)
	if nodeType == "formal" {
		if err := os.MkdirAll(securityDir, 0o700); err != nil {
			return err
		}
		fmt.Printf("Formal node private key path: %s\n", privateKeyPath)
		for {
			if _, err := os.Stat(privateKeyPath); err == nil {
				break
			}
			fmt.Printf("Put private key file at %s, then press Enter to continue.", privateKeyPath)
			_, _ = reader.ReadString('\n')
		}
	}

	if err := os.MkdirAll(filepath.Dir(*configPath), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return err
	}
	configContent := renderConfig(nodeName, nodeType, maxConcurrency, baseURL, nacosServer, nacosNamespace,
		rabbitHost, rabbitPort, rabbitUser, rabbitPassword, rabbitVhost, cacheRoot, privateKeyPath)
	if err := os.WriteFile(*configPath, []byte(configContent), 0o600); err != nil {
		return err
	}

	composeContent := renderCompose(image, *configPath, securityDir, cacheRoot, dockerNetwork)
	if err := os.WriteFile(*composePath, []byte(composeContent), 0o600); err != nil {
		return err
	}

	fmt.Printf("Config written: %s\n", *configPath)
	fmt.Printf("Compose written: %s\n", *composePath)
	if nodeType == "temp" {
		fmt.Printf("Run auth exchange before starting temp node: hnieoj-judge-node auth-exchange -config %s\n", *configPath)
	}
	fmt.Printf("Start command: docker compose -f %s up -d\n", *composePath)
	return nil
}

func runAuthExchange(args []string) error {
	fs := flag.NewFlagSet("auth-exchange", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file path")
	authCode := fs.String("auth-code", "", "temp auth code")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Node.Type != "temp" {
		return errors.New("auth-exchange only supports temp node")
	}
	code := strings.TrimSpace(*authCode)
	if code == "" {
		reader := bufio.NewReader(os.Stdin)
		code = promptRequired(reader, "Temp auth code")
	}
	cfg.HnieOJ.TempToken.AuthCode = code
	httpClient := &http.Client{Timeout: cfg.HnieOJ.RequestTimeout}
	cred, cache, err := auth.ExchangeTempToken(context.Background(), *cfg, httpClient)
	if err != nil {
		return err
	}
	if cfg.HnieOJ.TempToken.CachePath == "" {
		return errors.New("temp token cachePath is required")
	}
	if err := auth.SaveTempTokenCache(cfg.HnieOJ.TempToken.CachePath, cache); err != nil {
		return err
	}
	fmt.Printf("Temp token exchange succeeded, nodeId: %s, tokenId: %s, expireTime: %s\n",
		cred.NodeID, cred.TokenID, cred.ExpireTime.Format(time.RFC3339))
	fmt.Printf("Token cache written: %s\n", cfg.HnieOJ.TempToken.CachePath)
	return nil
}

func runConfigValidate(args []string) error {
	fs := flag.NewFlagSet("config-validate", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Node.Type == "formal" {
		if _, err := os.Stat(cfg.HnieOJ.FormalToken.PrivateKeyPath); err != nil {
			return fmt.Errorf("formal private key is not readable: %w", err)
		}
	}
	if cfg.Node.Type == "temp" {
		if _, err := auth.LoadTempTokenCache(cfg.HnieOJ.TempToken.CachePath); err != nil && cfg.HnieOJ.TempToken.AuthCode == "" {
			return fmt.Errorf("temp token cache is invalid and authCode is empty: %w", err)
		}
	}
	fmt.Println("Config validation succeeded")
	return nil
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	checkHTTP("HnieOJ backend", strings.TrimRight(cfg.HnieOJ.BaseURL, "/")+"/actuator/health", cfg.HnieOJ.RequestTimeout)
	checkTCP("RabbitMQ", cfg.RabbitMQ.Host, cfg.RabbitMQ.Port)
	checkHTTP("go-judge sandbox", strings.TrimRight(cfg.GoJudge.Endpoint, "/"), 5*time.Second)
	return nil
}

func prompt(reader *bufio.Reader, label string, defaultValue string) string {
	fmt.Printf("%s [%s]: ", label, defaultValue)
	value, _ := reader.ReadString('\n')
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultValue
	}
	return value
}

func promptRequired(reader *bufio.Reader, label string) string {
	for {
		fmt.Printf("%s: ", label)
		value, _ := reader.ReadString('\n')
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
		fmt.Println("This value is required.")
	}
}

func promptChoice(reader *bufio.Reader, label string, defaultValue string, choices []string) string {
	allowed := make(map[string]bool, len(choices))
	for _, choice := range choices {
		allowed[choice] = true
	}
	for {
		value := prompt(reader, label, defaultValue)
		if allowed[value] {
			return value
		}
		fmt.Printf("Allowed values: %s\n", strings.Join(choices, ", "))
	}
}

func promptInt(reader *bufio.Reader, label string, defaultValue int) int {
	for {
		value := prompt(reader, label, strconv.Itoa(defaultValue))
		n, err := strconv.Atoi(value)
		if err == nil && n > 0 {
			return n
		}
		fmt.Println("Please enter a positive integer.")
	}
}

func defaultHostname() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "judge-node-01"
	}
	return strings.TrimSpace(name)
}

func renderConfig(nodeName, nodeType string, maxConcurrency int, baseURL, nacosServer, nacosNamespace string,
	rabbitHost string, rabbitPort int, rabbitUser, rabbitPassword, rabbitVhost, cacheRoot, privateKeyPath string) string {
	return fmt.Sprintf(`node:
  name: %s
  type: %s
  maxConcurrency: %d

remoteConfig:
  enabled: true
  nacos:
    serverAddr: %s
    namespace: %s
    group: "HNIEOJ_JUDGE_GROUP"
    dataId: "hnieoj-judge-node.yaml"

hnieoj:
  baseUrl: %s
  requestTimeout: "30s"
  formalToken:
    privateKeyPath: %s
    cipherAlgorithm: "RSA/ECB/OAEPWithSHA-256AndMGF1Padding"
    refreshInterval: "30s"
    nacos:
      serverAddr: %s
      namespace: %s
      group: "HNIEOJ_SECRET_GROUP"
      dataId: "hnieoj-judge-formal-token.yaml"
  tempToken:
    authCode: ""
    cachePath: %s

rabbitmq:
  host: %s
  port: %d
  username: %s
  password: %s
  virtualHost: %s
  exchange: "hnieoj.judge.exchange"
  queue: "hnieoj.judge.task"
  routingKey: "judge.submission.created"
  deadLetterExchange: "hnieoj.judge.dlx"
  deadLetterQueue: "hnieoj.judge.task.dlq"
  deadLetterRoutingKey: "judge.submission.created.dlq"
  prefetch: %d
  maxRetries: 3
  retryBackoff: "10s"

testdata:
  cacheRoot: %s
  maxCacheBytes: 21474836480
  maxUnusedDuration: "72h"
  cleanupInterval: "1h"
  statsInterval: "5m"

gojudge:
  endpoint: "http://go-judge-sandbox:5050"
  authToken: ""

reporter:
  mode: "http"
  endpoint: "/judge/submissions/{submissionId}/events"

heartbeat:
  enabled: true
  endpoint: "/judge/nodes/heartbeat"
  interval: "30s"
`, quote(nodeName), quote(nodeType), maxConcurrency, quote(nacosServer), quote(nacosNamespace), quote(baseURL),
		quote(privateKeyPath), quote(nacosServer), quote(nacosNamespace), quote(defaultTokenCachePath), quote(rabbitHost),
		rabbitPort, quote(rabbitUser), quote(rabbitPassword), quote(rabbitVhost), maxConcurrency, quote(cacheRoot))
}

func renderCompose(image, configPath, securityDir, cacheRoot, dockerNetwork string) string {
	configDir := filepath.Dir(configPath)
	networkBlock := "    driver: bridge\n"
	if dockerNetwork != "private" {
		networkBlock = fmt.Sprintf("    external: true\n    name: %s\n", dockerNetwork)
	}
	return fmt.Sprintf(`services:
  go-judge-sandbox:
    image: %s
    restart: unless-stopped
    privileged: true
    shm_size: 512m
    command:
      - /usr/local/bin/go-judge
      - -http-addr=:5050
      - -mount-conf=/opt/go-judge/mount.yaml
    ports:
      - "5050:5050"
    networks:
      - hnieoj-judge

  hnieoj-judge-node:
    image: %s
    restart: unless-stopped
    command:
      - /usr/local/bin/hnieoj-judge-node
      - -config
      - /etc/hnieoj/go-judge/config.yaml
    volumes:
      - %s:/etc/hnieoj/go-judge:ro
      - %s:/etc/hnieoj/judge-security:ro
      - %s:/data/oj/judge-cache
    networks:
      - hnieoj-judge

networks:
  hnieoj-judge:
%s`, image, image, configDir, securityDir, cacheRoot, networkBlock)
}

func quote(value string) string {
	return strconv.Quote(value)
}

func checkHTTP(name, url string, timeout time.Duration) {
	client := http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Printf("%s check failed: %v\n", name, err)
		return
	}
	defer resp.Body.Close()
	fmt.Printf("%s check status: %d\n", name, resp.StatusCode)
}

func checkTCP(name, host string, port int) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		fmt.Printf("%s TCP check failed: %v\n", name, err)
		return
	}
	_ = conn.Close()
	fmt.Printf("%s TCP check succeeded\n", name)
}
