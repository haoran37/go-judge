package auth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/goccy/go-yaml"
)

const tempTokenProofTypeEd25519 = "ed25519"

type Credential struct {
	mu              sync.RWMutex
	HeaderName      string
	HeaderValue     string
	NodeID          string
	TokenID         string
	ExpireTime      time.Time
	InstanceID      string
	FingerprintHash string
	ProofType       string
	InstanceSecret  []byte
}

func (c *Credential) Apply(req *http.Request) {
	c.mu.RLock()
	headerName := c.HeaderName
	headerValue := c.HeaderValue
	nodeID := c.NodeID
	tokenID := c.TokenID
	instanceID := c.InstanceID
	fingerprintHash := c.FingerprintHash
	proofType := c.ProofType
	instanceSecret := append([]byte(nil), c.InstanceSecret...)
	c.mu.RUnlock()
	if headerName != "" && headerValue != "" {
		req.Header.Set(headerName, headerValue)
	}
	if nodeID != "" {
		req.Header.Set("X-Judge-Node-Id", nodeID)
	}
	if tokenID != "" {
		req.Header.Set("X-Judge-Token-Id", tokenID)
	}
	if instanceID != "" {
		req.Header.Set("X-Judge-Instance-Id", instanceID)
	}
	if fingerprintHash != "" {
		req.Header.Set("X-Judge-Fingerprint", fingerprintHash)
	}
	if len(instanceSecret) > 0 {
		signRequest(req, instanceSecret, proofType)
	}
}

func (c *Credential) Expired(now time.Time) bool {
	expireTime := c.ExpiresAt()
	return !expireTime.IsZero() && !now.Before(expireTime)
}

func (c *Credential) SetHeaderValue(value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.HeaderValue = value
}

func (c *Credential) ExpiresAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ExpireTime
}

func (c *Credential) Replace(next *Credential) {
	if next == nil {
		return
	}
	next.mu.RLock()
	headerName := next.HeaderName
	headerValue := next.HeaderValue
	nodeID := next.NodeID
	tokenID := next.TokenID
	expireTime := next.ExpireTime
	instanceID := next.InstanceID
	fingerprintHash := next.FingerprintHash
	proofType := next.ProofType
	instanceSecret := append([]byte(nil), next.InstanceSecret...)
	next.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.HeaderName = headerName
	c.HeaderValue = headerValue
	c.NodeID = nodeID
	c.TokenID = tokenID
	c.ExpireTime = expireTime
	c.InstanceID = instanceID
	c.FingerprintHash = fingerprintHash
	c.ProofType = proofType
	c.InstanceSecret = instanceSecret
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
		cred, err := resolveTempCredential(ctx, cfg, client)
		if err != nil {
			return nil, err
		}
		startTempTokenRefresher(ctx, cfg, client, cred)
		return cred, nil
	default:
		return nil, fmt.Errorf("unsupported node type %q", cfg.Node.Type)
	}
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

func startTempTokenRefresher(ctx context.Context, cfg config.Config, client *http.Client, cred *Credential) {
	if cfg.HnieOJ.TempToken.AuthCode == "" {
		return
	}
	go func() {
		for {
			wait := tempRefreshDelay(cred.ExpiresAt(), time.Now())
			if wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			next, err := exchangeTempToken(ctx, cfg, client)
			if err != nil {
				timer := time.NewTimer(30 * time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				continue
			}
			cred.Replace(next)
		}
	}()
}

func resolveTempCredential(ctx context.Context, cfg config.Config, client *http.Client) (*Credential, error) {
	if strings.TrimSpace(cfg.HnieOJ.TempToken.JWT) != "" {
		return credentialFromTempTokenConfig(cfg.HnieOJ.TempToken)
	}
	return exchangeTempToken(ctx, cfg, client)
}

func ExchangeTempToken(ctx context.Context, cfg config.Config, client *http.Client) (*Credential, error) {
	return exchangeTempToken(ctx, cfg, client)
}

func credentialFromTempTokenConfig(cfg config.TempToken) (*Credential, error) {
	token := strings.TrimSpace(cfg.JWT)
	if token == "" {
		return nil, errors.New("temp jwt is required")
	}
	tokenType := strings.TrimSpace(cfg.TokenType)
	if tokenType == "" {
		tokenType = "Bearer"
	}
	expireTime, err := parseExpireTime(cfg.ExpireTime)
	if err != nil {
		return nil, err
	}
	binding, err := buildTempNodeBinding(config.Config{HnieOJ: config.HnieOJConfig{TempToken: cfg}})
	if err != nil {
		return nil, err
	}
	fingerprintHash := strings.TrimSpace(cfg.FingerprintHash)
	if fingerprintHash == "" && binding != nil {
		fingerprintHash = binding.FingerprintHash
	}
	return &Credential{
		HeaderName:      "Authorization",
		HeaderValue:     tokenType + " " + token,
		NodeID:          strings.TrimSpace(cfg.NodeID),
		TokenID:         strings.TrimSpace(cfg.TokenID),
		ExpireTime:      expireTime,
		InstanceID:      bindingInstanceID(binding),
		FingerprintHash: fingerprintHash,
		ProofType:       bindingProofType(binding, cfg.ProofType),
		InstanceSecret:  bindingSecret(binding),
	}, nil
}

func tempRefreshDelay(expireTime, now time.Time) time.Duration {
	if expireTime.IsZero() {
		return time.Hour
	}
	refreshAt := expireTime.Add(-time.Minute)
	if !refreshAt.After(now) {
		return 0
	}
	return refreshAt.Sub(now)
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

func buildTempNodeBinding(cfg config.Config) (*tempNodeBinding, error) {
	temp := cfg.HnieOJ.TempToken
	instanceID := strings.TrimSpace(temp.InstanceID)
	secretPath := strings.TrimSpace(temp.InstanceSecretPath)
	if instanceID == "" && secretPath == "" {
		return nil, nil
	}
	proofType := strings.TrimSpace(temp.ProofType)
	if proofType == "" {
		proofType = tempTokenProofTypeEd25519
	}
	if proofType != tempTokenProofTypeEd25519 {
		return nil, fmt.Errorf("unsupported temp token proof type %q", proofType)
	}
	var secret []byte
	if secretPath != "" {
		b, err := os.ReadFile(secretPath)
		if err != nil {
			return nil, err
		}
		secret = []byte(strings.TrimSpace(string(b)))
		if len(secret) == 0 {
			return nil, fmt.Errorf("temp instance secret file %q is empty", secretPath)
		}
	}
	fingerprint := buildTempNodeFingerprint(instanceID, cfg.Node.Name, cfg.Node.SupportedJudgeModes, time.Now())
	proof := tempNodeProof{Type: proofType}
	if len(secret) > 0 {
		proof.PublicKey = ed25519PublicKey(secret)
	}
	return &tempNodeBinding{
		Fingerprint:     fingerprint,
		Proof:           proof,
		FingerprintHash: hashFingerprint(fingerprint),
		InstanceSecret:  secret,
	}, nil
}

func buildTempNodeFingerprint(instanceID, nodeName string, supportedModes []string, now time.Time) tempNodeFingerprint {
	hostname, _ := os.Hostname()
	macHashes, ipHashes := networkIdentityHashes()
	machineHash := machineIDHash()
	if machineHash == "" {
		machineHash = hashString("hnieoj-temp-instance:" + strings.TrimSpace(instanceID))
	}
	if machineHash == "" {
		machineHash = hashString("hnieoj-hostname:" + hostname)
	}
	return tempNodeFingerprint{
		InstanceID:          strings.TrimSpace(instanceID),
		NodeName:            strings.TrimSpace(nodeName),
		HostnameHash:        hashString(hostname),
		MachineIDHash:       machineHash,
		MACAddressHashes:    macHashes,
		IPAddressHashes:     ipHashes,
		SupportedJudgeModes: append([]string(nil), supportedModes...),
		ClientTime:          now.UTC().Format(time.RFC3339),
	}
}

func hashFingerprint(fingerprint tempNodeFingerprint) string {
	stable := fingerprint
	stable.ClientTime = ""
	body, err := json.Marshal(stable)
	if err != nil {
		return ""
	}
	return hashBytes(body)
}

func machineIDHash() string {
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		b, err := os.ReadFile(path)
		if err == nil {
			if value := strings.TrimSpace(string(b)); value != "" {
				return hashString(value)
			}
		}
	}
	return ""
}

func networkIdentityHashes() ([]string, []string) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, nil
	}
	macSet := make(map[string]struct{})
	ipSet := make(map[string]struct{})
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		if len(iface.HardwareAddr) > 0 {
			macSet[hashString(iface.HardwareAddr.String())] = struct{}{}
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ipSet[hashString(ip.String())] = struct{}{}
		}
	}
	return sortedKeys(macSet), sortedKeys(ipSet)
}

func sortedKeys(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func hashString(value string) string {
	if value == "" {
		return ""
	}
	return hashBytes([]byte(value))
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func ed25519PrivateKey(secret []byte) ed25519.PrivateKey {
	seed := sha256.Sum256(secret)
	return ed25519.NewKeyFromSeed(seed[:])
}

func ed25519PublicKey(secret []byte) string {
	privateKey := ed25519PrivateKey(secret)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return base64.StdEncoding.EncodeToString(publicKey)
}

func bindingInstanceID(binding *tempNodeBinding) string {
	if binding == nil {
		return ""
	}
	return binding.Fingerprint.InstanceID
}

func bindingProofType(binding *tempNodeBinding, fallback string) string {
	if binding != nil && binding.Proof.Type != "" {
		return binding.Proof.Type
	}
	if fallback != "" {
		return fallback
	}
	return tempTokenProofTypeEd25519
}

func bindingSecret(binding *tempNodeBinding) []byte {
	if binding == nil || len(binding.InstanceSecret) == 0 {
		return nil
	}
	return append([]byte(nil), binding.InstanceSecret...)
}

func signRequest(req *http.Request, secret []byte, proofType string) {
	if proofType == "" {
		proofType = tempTokenProofTypeEd25519
	}
	if proofType != tempTokenProofTypeEd25519 {
		return
	}
	timestamp := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	nonce := randomNonce()
	bodyHash := requestBodyHash(req)
	target := req.URL.RequestURI()
	if target == "" {
		target = "/"
	}
	payload := strings.Join([]string{
		req.Method,
		target,
		bodyHash,
		timestamp,
		nonce,
	}, "\n")
	req.Header.Set("X-Judge-Signature-Algorithm", proofType)
	req.Header.Set("X-Judge-Timestamp", timestamp)
	req.Header.Set("X-Judge-Nonce", nonce)
	req.Header.Set("X-Judge-Body-Sha256", bodyHash)
	signature := ed25519.Sign(ed25519PrivateKey(secret), []byte(payload))
	req.Header.Set("X-Judge-Signature", base64.StdEncoding.EncodeToString(signature))
}

func randomNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func requestBodyHash(req *http.Request) string {
	if req.Body == nil || req.Body == http.NoBody {
		return hashBytes([]byte{})
	}
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err == nil {
			defer body.Close()
			b, err := io.ReadAll(body)
			if err == nil {
				return hashBytes(b)
			}
		}
	}
	b, err := io.ReadAll(req.Body)
	if err != nil {
		req.Body = io.NopCloser(bytes.NewReader(nil))
		return hashBytes([]byte{})
	}
	req.Body = io.NopCloser(bytes.NewReader(b))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	return hashBytes(b)
}

type tempNodeFingerprint struct {
	InstanceID          string   `json:"instanceId,omitempty"`
	NodeName            string   `json:"nodeName,omitempty"`
	HostnameHash        string   `json:"hostnameHash,omitempty"`
	MachineIDHash       string   `json:"machineIdHash,omitempty"`
	MACAddressHashes    []string `json:"macAddressHashes,omitempty"`
	IPAddressHashes     []string `json:"ipAddressHashes,omitempty"`
	SupportedJudgeModes []string `json:"supportedJudgeModes,omitempty"`
	ClientTime          string   `json:"clientTime,omitempty"`
}

type tempNodeProof struct {
	Type       string `json:"type,omitempty"`
	SecretHash string `json:"secretHash,omitempty"`
	PublicKey  string `json:"publicKey,omitempty"`
}

type tempNodeBinding struct {
	Fingerprint     tempNodeFingerprint
	Proof           tempNodeProof
	FingerprintHash string
	InstanceSecret  []byte
}

type tempTokenRequest struct {
	AuthCode    string               `json:"authCode"`
	NodeName    string               `json:"nodeName"`
	Fingerprint *tempNodeFingerprint `json:"fingerprint,omitempty"`
	Proof       *tempNodeProof       `json:"proof,omitempty"`
}

type tempTokenResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Token           string `json:"token"`
		TokenType       string `json:"tokenType"`
		NodeID          string `json:"nodeId"`
		TokenID         string `json:"tokenId"`
		ExpireTime      string `json:"expireTime"`
		FingerprintHash string `json:"fingerprintHash"`
	} `json:"data"`
}

func exchangeTempToken(ctx context.Context, cfg config.Config, client *http.Client) (*Credential, error) {
	if cfg.HnieOJ.TempToken.AuthCode == "" {
		return nil, errors.New("temp auth code is required")
	}
	tokenRequest := tempTokenRequest{
		AuthCode: cfg.HnieOJ.TempToken.AuthCode,
		NodeName: cfg.Node.Name,
	}
	binding, err := buildTempNodeBinding(cfg)
	if err != nil {
		return nil, err
	}
	if binding != nil {
		tokenRequest.Fingerprint = &binding.Fingerprint
		tokenRequest.Proof = &binding.Proof
	}
	body, err := json.Marshal(tokenRequest)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.HnieOJ.BaseURL, "/")+"/api/judge/temp-token", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("temp token exchange failed with status %d", resp.StatusCode)
	}
	var out tempTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Code != 200 || out.Data.Token == "" {
		return nil, fmt.Errorf("temp token exchange failed: %s", out.Msg)
	}
	tokenType := out.Data.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	expireTime, err := parseExpireTime(out.Data.ExpireTime)
	if err != nil {
		return nil, err
	}
	fingerprintHash := strings.TrimSpace(out.Data.FingerprintHash)
	if fingerprintHash == "" {
		fingerprintHash = strings.TrimSpace(cfg.HnieOJ.TempToken.FingerprintHash)
	}
	if fingerprintHash == "" && binding != nil {
		fingerprintHash = binding.FingerprintHash
	}
	return &Credential{
		HeaderName:      "Authorization",
		HeaderValue:     tokenType + " " + out.Data.Token,
		NodeID:          out.Data.NodeID,
		TokenID:         out.Data.TokenID,
		ExpireTime:      expireTime,
		InstanceID:      bindingInstanceID(binding),
		FingerprintHash: fingerprintHash,
		ProofType:       bindingProofType(binding, cfg.HnieOJ.TempToken.ProofType),
		InstanceSecret:  bindingSecret(binding),
	}, nil
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
