package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/goccy/go-yaml"
)

type Credential struct {
	mu          sync.RWMutex
	HeaderName  string
	HeaderValue string
	NodeID      string
	TokenID     string
	ExpireTime  time.Time
}

func (c *Credential) Apply(req *http.Request) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.HeaderName != "" && c.HeaderValue != "" {
		req.Header.Set(c.HeaderName, c.HeaderValue)
	}
}

func (c *Credential) Expired(now time.Time) bool {
	return !c.ExpireTime.IsZero() && !now.Before(c.ExpireTime)
}

func (c *Credential) SetHeaderValue(value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.HeaderValue = value
}

func Authenticate(ctx context.Context, cfg config.Config, client *http.Client) (*Credential, error) {
	switch cfg.Node.Type {
	case "formal":
		token, err := resolveFormalToken(ctx, cfg.HnieOJ.FormalToken, client)
		if err != nil {
			return nil, err
		}
		cred := &Credential{HeaderName: "X-Judge-Token", HeaderValue: token, NodeID: cfg.Node.Name}
		startFormalTokenRefresher(ctx, cfg.HnieOJ.FormalToken, client, cred)
		return cred, nil
	case "temp":
		return ResolveTempCredential(ctx, cfg, client)
	default:
		return nil, fmt.Errorf("unsupported node type %q", cfg.Node.Type)
	}
}

type TempTokenCache struct {
	Token      string `json:"token"`
	TokenType  string `json:"tokenType"`
	NodeID     string `json:"nodeId"`
	TokenID    string `json:"tokenId"`
	ExpireTime string `json:"expireTime"`
}

func ResolveTempCredential(ctx context.Context, cfg config.Config, client *http.Client) (*Credential, error) {
	if cred, err := LoadTempTokenCache(cfg.HnieOJ.TempToken.CachePath); err == nil {
		return cred, nil
	}
	if cfg.HnieOJ.TempToken.AuthCode == "" {
		return nil, errors.New("temp auth code or valid temp token cache is required")
	}
	cred, cache, err := ExchangeTempToken(ctx, cfg, client)
	if err != nil {
		return nil, err
	}
	if cfg.HnieOJ.TempToken.CachePath != "" {
		if err := SaveTempTokenCache(cfg.HnieOJ.TempToken.CachePath, cache); err != nil {
			return nil, err
		}
	}
	return cred, nil
}

func LoadTempTokenCache(path string) (*Credential, error) {
	if path == "" {
		return nil, errors.New("temp token cache path is empty")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cache TempTokenCache
	if err := json.Unmarshal(b, &cache); err != nil {
		return nil, err
	}
	return credentialFromTempTokenCache(cache)
}

func SaveTempTokenCache(path string, cache TempTokenCache) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func credentialFromTempTokenCache(cache TempTokenCache) (*Credential, error) {
	token := strings.TrimSpace(cache.Token)
	if token == "" {
		return nil, errors.New("temp token cache token is empty")
	}
	tokenType := strings.TrimSpace(cache.TokenType)
	if tokenType == "" {
		tokenType = "Bearer"
	}
	expireTime, err := parseExpireTime(cache.ExpireTime)
	if err != nil {
		return nil, err
	}
	if !expireTime.IsZero() && !time.Now().Before(expireTime) {
		return nil, errors.New("temp token cache is expired")
	}
	return &Credential{
		HeaderName:  "Authorization",
		HeaderValue: tokenType + " " + token,
		NodeID:      strings.TrimSpace(cache.NodeID),
		TokenID:     strings.TrimSpace(cache.TokenID),
		ExpireTime:  expireTime,
	}, nil
}

func resolveFormalToken(ctx context.Context, cfg config.FormalToken, client *http.Client) (string, error) {
	if cfg.Nacos.ServerAddr != "" && cfg.Nacos.DataID != "" && cfg.Nacos.Group != "" {
		encryptedToken, err := fetchEncryptedTokenFromNacos(ctx, cfg, client)
		if err == nil {
			return decryptFormalToken(cfg, encryptedToken)
		}
		if isPlaceholderToken(cfg.EncryptedToken) {
			return "", err
		}
	}

	encryptedToken := strings.TrimSpace(cfg.EncryptedToken)
	if isPlaceholderToken(encryptedToken) {
		return "", errors.New("formal encrypted token is required")
	}
	return decryptFormalToken(cfg, encryptedToken)
}

func isPlaceholderToken(value string) bool {
	normalized := strings.TrimSpace(value)
	return normalized == "" || normalized == "replace_me" || normalized == "{rsa}Base64CipherText"
}

func startFormalTokenRefresher(ctx context.Context, cfg config.FormalToken, client *http.Client, cred *Credential) {
	if cfg.Nacos.ServerAddr == "" || cfg.Nacos.DataID == "" || cfg.Nacos.Group == "" {
		return
	}
	interval := cfg.RefreshInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				token, err := resolveFormalToken(ctx, cfg, client)
				if err != nil {
					continue
				}
				cred.SetHeaderValue(token)
			}
		}
	}()
}

func decryptFormalToken(cfg config.FormalToken, encryptedToken string) (string, error) {
	if encryptedToken == "" {
		return "", errors.New("formal encrypted token is required")
	}
	if cfg.PrivateKeyPath == "" {
		return "", errors.New("formal private key path is required")
	}
	if cfg.CipherAlgorithm != "" && cfg.CipherAlgorithm != "RSA/ECB/OAEPWithSHA-256AndMGF1Padding" {
		return "", fmt.Errorf("unsupported cipher algorithm %q", cfg.CipherAlgorithm)
	}
	privateKey, err := readPrivateKey(cfg.PrivateKeyPath)
	if err != nil {
		return "", err
	}
	cipherText := strings.TrimPrefix(encryptedToken, "{rsa}")
	raw, err := base64.StdEncoding.DecodeString(cipherText)
	if err != nil {
		return "", err
	}
	plain, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, privateKey, raw, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func readPrivateKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("invalid PEM private key")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("private key is not RSA")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return key, nil
}

type formalTokenConfigResponse struct {
	HnieOJ struct {
		Judge struct {
			FormalToken struct {
				EncryptedToken string `yaml:"encrypted-token"`
			} `yaml:"formal-token"`
		} `yaml:"judge"`
	} `yaml:"hnieoj"`
}

func fetchEncryptedTokenFromNacos(ctx context.Context, cfg config.FormalToken, client *http.Client) (string, error) {
	if cfg.Nacos.ServerAddr == "" || cfg.Nacos.DataID == "" || cfg.Nacos.Group == "" {
		return "", errors.New("formal token nacos config is required")
	}
	baseURL := strings.TrimRight(cfg.Nacos.ServerAddr, "/")
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "http://" + baseURL
	}
	values := url.Values{}
	values.Set("dataId", cfg.Nacos.DataID)
	values.Set("group", cfg.Nacos.Group)
	if cfg.Nacos.Namespace != "" {
		values.Set("tenant", cfg.Nacos.Namespace)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/nacos/v1/cs/configs?"+values.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch formal token from nacos failed with status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var out formalTokenConfigResponse
	if err := yaml.Unmarshal(body, &out); err != nil {
		return "", err
	}
	encryptedToken := strings.TrimSpace(out.HnieOJ.Judge.FormalToken.EncryptedToken)
	if encryptedToken == "" {
		return "", errors.New("formal encrypted token is empty in nacos")
	}
	return encryptedToken, nil
}

type tempTokenRequest struct {
	AuthCode string `json:"authCode"`
	NodeName string `json:"nodeName"`
}

type tempTokenResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Token      string `json:"token"`
		TokenType  string `json:"tokenType"`
		NodeID     string `json:"nodeId"`
		TokenID    string `json:"tokenId"`
		ExpireTime string `json:"expireTime"`
	} `json:"data"`
}

func ExchangeTempToken(ctx context.Context, cfg config.Config, client *http.Client) (*Credential, TempTokenCache, error) {
	if cfg.HnieOJ.TempToken.AuthCode == "" {
		return nil, TempTokenCache{}, errors.New("temp auth code is required")
	}
	body, err := json.Marshal(tempTokenRequest{
		AuthCode: cfg.HnieOJ.TempToken.AuthCode,
		NodeName: cfg.Node.Name,
	})
	if err != nil {
		return nil, TempTokenCache{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.HnieOJ.BaseURL, "/")+"/api/judge/temp-token", bytes.NewReader(body))
	if err != nil {
		return nil, TempTokenCache{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, TempTokenCache{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, TempTokenCache{}, fmt.Errorf("temp token exchange failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out tempTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, TempTokenCache{}, err
	}
	if out.Code != 200 || out.Data.Token == "" {
		return nil, TempTokenCache{}, fmt.Errorf("temp token exchange failed: %s", out.Msg)
	}
	tokenType := out.Data.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	expireTime, err := parseExpireTime(out.Data.ExpireTime)
	if err != nil {
		return nil, TempTokenCache{}, err
	}
	cache := TempTokenCache{
		Token:      out.Data.Token,
		TokenType:  tokenType,
		NodeID:     out.Data.NodeID,
		TokenID:    out.Data.TokenID,
		ExpireTime: out.Data.ExpireTime,
	}
	return &Credential{
		HeaderName:  "Authorization",
		HeaderValue: tokenType + " " + out.Data.Token,
		NodeID:      out.Data.NodeID,
		TokenID:     out.Data.TokenID,
		ExpireTime:  expireTime,
	}, cache, nil
}

func parseExpireTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02T15:04:05", s, time.Local)
}
