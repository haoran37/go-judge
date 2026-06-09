package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/node"
)

const sessionCookie = "hnieoj_judge_session"

type Server struct {
	store      *Store
	manager    *node.Manager
	logs       *logging.Recorder
	sessionsMu sync.Mutex
	sessions   map[string]time.Time
}

type ConfigDTO struct {
	Node      NodeDTO      `json:"node"`
	HnieOJ    HnieOJDTO    `json:"hnieoj"`
	RabbitMQ  RabbitMQDTO  `json:"rabbitmq"`
	Testdata  TestdataDTO  `json:"testdata"`
	GoJudge   GoJudgeDTO   `json:"gojudge"`
	Reporter  ReporterDTO  `json:"reporter"`
	Heartbeat HeartbeatDTO `json:"heartbeat"`
	Remote    RemoteDTO    `json:"remoteConfig"`
}

type NodeDTO struct {
	Name                string   `json:"name"`
	Type                string   `json:"type"`
	MaxConcurrency      int      `json:"maxConcurrency"`
	SupportedJudgeModes []string `json:"supportedJudgeModes"`
}

type HnieOJDTO struct {
	BaseURL        string         `json:"baseUrl"`
	RequestTimeout string         `json:"requestTimeout"`
	FormalToken    FormalTokenDTO `json:"formalToken"`
	TempToken      TempTokenDTO   `json:"tempToken"`
}

type FormalTokenDTO struct {
	PrivateKeyConfigured bool     `json:"privateKeyConfigured"`
	CipherAlgorithm      string   `json:"cipherAlgorithm"`
	RefreshInterval      string   `json:"refreshInterval"`
	Nacos                NacosDTO `json:"nacos"`
}

type NacosDTO struct {
	ServerAddr string `json:"serverAddr"`
	Namespace  string `json:"namespace"`
	Group      string `json:"group"`
	DataID     string `json:"dataId"`
}

type TempTokenDTO struct {
	AuthCode           string `json:"authCode,omitempty"`
	TokenType          string `json:"tokenType"`
	NodeID             string `json:"nodeId"`
	TokenID            string `json:"tokenId"`
	ExpireTime         string `json:"expireTime"`
	InstanceID         string `json:"instanceId"`
	FingerprintHash    string `json:"fingerprintHash"`
	ProofType          string `json:"proofType"`
	InstanceConfigured bool   `json:"instanceConfigured"`
}

type RabbitMQDTO struct {
	Host                 string `json:"host"`
	Port                 int    `json:"port"`
	Username             string `json:"username"`
	Password             string `json:"password,omitempty"`
	PasswordConfigured   bool   `json:"passwordConfigured"`
	VirtualHost          string `json:"virtualHost"`
	Exchange             string `json:"exchange"`
	Queue                string `json:"queue"`
	RoutingKey           string `json:"routingKey"`
	DeadLetterExchange   string `json:"deadLetterExchange"`
	DeadLetterQueue      string `json:"deadLetterQueue"`
	DeadLetterRoutingKey string `json:"deadLetterRoutingKey"`
	Prefetch             int    `json:"prefetch"`
	MaxRetries           int    `json:"maxRetries"`
	RetryBackoff         string `json:"retryBackoff"`
}

type TestdataDTO struct {
	CacheRoot         string `json:"cacheRoot"`
	MaxCacheBytes     int64  `json:"maxCacheBytes"`
	MaxUnusedDuration string `json:"maxUnusedDuration"`
	CleanupInterval   string `json:"cleanupInterval"`
	StatsInterval     string `json:"statsInterval"`
}

type GoJudgeDTO struct {
	Endpoint            string `json:"endpoint"`
	AuthToken           string `json:"authToken,omitempty"`
	AuthTokenConfigured bool   `json:"authTokenConfigured"`
}

type ReporterDTO struct {
	Mode     string `json:"mode"`
	Endpoint string `json:"endpoint"`
}

type HeartbeatDTO struct {
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint"`
	Interval string `json:"interval"`
}

type RemoteDTO struct {
	Enabled bool     `json:"enabled"`
	Nacos   NacosDTO `json:"nacos"`
}

type setupStatusResponse struct {
	AdminInitialized bool        `json:"adminInitialized"`
	Authenticated    bool        `json:"authenticated"`
	Configured       bool        `json:"configured"`
	Runtime          node.Status `json:"runtime"`
}

func NewServer(store *Store, manager *node.Manager, logs *logging.Recorder) *Server {
	return &Server{store: store, manager: manager, logs: logs, sessions: map[string]time.Time{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/setup/status", s.handleSetupStatus)
	mux.HandleFunc("/api/v1/setup/admin", s.handleSetupAdmin)
	mux.HandleFunc("/api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("/api/v1/auth/logout", s.withAuth(s.handleLogout))
	mux.HandleFunc("/api/v1/auth/me", s.withAuth(s.handleMe))
	mux.HandleFunc("/api/v1/config", s.withAuth(s.handleConfig))
	mux.HandleFunc("/api/v1/setup/formal", s.withAuth(s.handleSetupFormal))
	mux.HandleFunc("/api/v1/setup/temp/exchange", s.withAuth(s.handleSetupTemp))
	mux.HandleFunc("/api/v1/runtime/start", s.withAuth(s.handleStart))
	mux.HandleFunc("/api/v1/runtime/stop", s.withAuth(s.handleStop))
	mux.HandleFunc("/api/v1/runtime/restart", s.withAuth(s.handleRestart))
	mux.HandleFunc("/api/v1/status", s.withAuth(s.handleStatus))
	mux.HandleFunc("/api/v1/metrics/summary", s.withAuth(s.handleStatus))
	mux.HandleFunc("/api/v1/logs/recent", s.withAuth(s.handleLogs))
	mux.Handle("/", StaticHandler())
	return mux
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	_, configured, _ := s.store.LoadConfig()
	authenticated := false
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		authenticated = s.validSession(cookie.Value)
	}
	writeJSON(w, setupStatusResponse{
		AdminInitialized: s.store.AdminInitialized(),
		Authenticated:    authenticated,
		Configured:       configured,
		Runtime:          s.manager.Status(),
	})
}

func (s *Server) handleSetupAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.store.AdminInitialized() {
		http.Error(w, "admin is already initialized", http.StatusConflict)
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Password) < 8 {
		http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	if err := s.store.SaveAdminPassword(req.Password); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.issueSession(w)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.store.VerifyPassword(req.Password) {
		http.Error(w, "invalid password", http.StatusUnauthorized)
		return
	}
	s.issueSession(w)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookie)
	if err == nil {
		s.sessionsMu.Lock()
		delete(s.sessions, cookie.Value)
		s.sessionsMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]bool{"authenticated": true})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, ok := s.manager.Config()
		if !ok {
			stored, exists, err := s.store.LoadConfig()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if !exists {
				cfg = config.Default()
			} else {
				cfg = stored
			}
		}
		writeJSON(w, configToDTO(*cfg))
	case http.MethodPut:
		current, exists, err := s.store.LoadConfig()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !exists {
			current = config.Default()
		}
		var dto ConfigDTO
		if err := readJSON(r, &dto); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cfg, err := dtoToConfig(dto, *current)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := cfg.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.store.SaveConfig(*cfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.manager.SetConfig(*cfg)
		writeJSON(w, map[string]bool{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleSetupFormal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Config        ConfigDTO `json:"config"`
		PrivateKeyPEM string    `json:"privateKeyPem"`
	}
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	base := *config.Default()
	cfg, err := dtoToConfig(req.Config, base)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	keyPath, err := s.store.SaveFormalPrivateKey(req.PrivateKeyPEM)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfg.Node.Type = "formal"
	cfg.HnieOJ.FormalToken.PrivateKeyPath = keyPath
	cfg.HnieOJ.TempToken = config.TempToken{ProofType: "hmac-sha256"}
	if err := cfg.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.SaveConfig(*cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.manager.SetConfig(*cfg)
	writeJSON(w, map[string]any{"ok": true, "config": configToDTO(*cfg)})
}

func (s *Server) handleSetupTemp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Config   ConfigDTO `json:"config"`
		AuthCode string    `json:"authCode"`
	}
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfg, err := dtoToConfig(req.Config, *config.Default())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	instanceID, secretPath, err := s.store.EnsureTempIdentity()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cfg.Node.Type = "temp"
	cfg.HnieOJ.TempToken.AuthCode = req.AuthCode
	cfg.HnieOJ.TempToken.InstanceID = instanceID
	cfg.HnieOJ.TempToken.InstanceSecretPath = secretPath
	cfg.HnieOJ.TempToken.ProofType = "hmac-sha256"
	cfg.HnieOJ.FormalToken.PrivateKeyPath = ""
	if err := cfg.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	httpClient := &http.Client{Timeout: cfg.HnieOJ.RequestTimeout}
	cred, err := auth.ExchangeTempToken(r.Context(), *cfg, httpClient)
	if err != nil {
		http.Error(w, "临时授权码兑换失败："+err.Error(), http.StatusBadRequest)
		return
	}
	fillTempCredential(cfg, cred)
	if err := s.store.SaveConfig(*cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.manager.SetConfig(*cfg)
	writeJSON(w, map[string]any{"ok": true, "config": configToDTO(*cfg)})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.manager.Start(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.manager.Status())
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.manager.Stop(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.manager.Status())
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	if err := s.manager.Restart(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.manager.Status())
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.manager.Status())
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.logs.Recent())
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.store.AdminInitialized() {
			http.Error(w, "admin is not initialized", http.StatusUnauthorized)
			return
		}
		cookie, err := r.Cookie(sessionCookie)
		if err != nil || !s.validSession(cookie.Value) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) issueSession(w http.ResponseWriter) {
	token := randomToken(32)
	expires := time.Now().Add(24 * time.Hour)
	s.sessionsMu.Lock()
	s.sessions[token] = expires
	s.sessionsMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) validSession(token string) bool {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	expires, ok := s.sessions[token]
	if !ok || time.Now().After(expires) {
		delete(s.sessions, token)
		return false
	}
	return true
}

func fillTempCredential(cfg *config.Config, cred *auth.Credential) {
	cfg.HnieOJ.TempToken.NodeID = cred.NodeID
	cfg.HnieOJ.TempToken.TokenID = cred.TokenID
	cfg.HnieOJ.TempToken.ExpireTime = cred.ExpireTime.Format(time.RFC3339)
	cfg.HnieOJ.TempToken.FingerprintHash = cred.FingerprintHash
	tokenType := "Bearer"
	value := strings.TrimSpace(cred.HeaderValue)
	if strings.Contains(value, " ") {
		parts := strings.SplitN(value, " ", 2)
		tokenType = parts[0]
		value = parts[1]
	}
	cfg.HnieOJ.TempToken.TokenType = tokenType
	cfg.HnieOJ.TempToken.JWT = value
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func parseDuration(value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	return time.ParseDuration(value)
}

func configToDTO(cfg config.Config) ConfigDTO {
	return ConfigDTO{
		Node: NodeDTO{
			Name:                cfg.Node.Name,
			Type:                cfg.Node.Type,
			MaxConcurrency:      cfg.Node.MaxConcurrency,
			SupportedJudgeModes: cfg.Node.SupportedJudgeModes,
		},
		HnieOJ: HnieOJDTO{
			BaseURL:        cfg.HnieOJ.BaseURL,
			RequestTimeout: cfg.HnieOJ.RequestTimeout.String(),
			FormalToken: FormalTokenDTO{
				PrivateKeyConfigured: cfg.HnieOJ.FormalToken.PrivateKeyPath != "",
				CipherAlgorithm:      cfg.HnieOJ.FormalToken.CipherAlgorithm,
				RefreshInterval:      cfg.HnieOJ.FormalToken.RefreshInterval.String(),
				Nacos:                nacosToDTO(cfg.HnieOJ.FormalToken.Nacos),
			},
			TempToken: TempTokenDTO{
				TokenType:          cfg.HnieOJ.TempToken.TokenType,
				NodeID:             cfg.HnieOJ.TempToken.NodeID,
				TokenID:            cfg.HnieOJ.TempToken.TokenID,
				ExpireTime:         cfg.HnieOJ.TempToken.ExpireTime,
				InstanceID:         cfg.HnieOJ.TempToken.InstanceID,
				FingerprintHash:    cfg.HnieOJ.TempToken.FingerprintHash,
				ProofType:          cfg.HnieOJ.TempToken.ProofType,
				InstanceConfigured: cfg.HnieOJ.TempToken.InstanceSecretPath != "",
			},
		},
		RabbitMQ: RabbitMQDTO{
			Host:                 cfg.RabbitMQ.Host,
			Port:                 cfg.RabbitMQ.Port,
			Username:             cfg.RabbitMQ.Username,
			PasswordConfigured:   cfg.RabbitMQ.Password != "",
			VirtualHost:          cfg.RabbitMQ.VirtualHost,
			Exchange:             cfg.RabbitMQ.Exchange,
			Queue:                cfg.RabbitMQ.Queue,
			RoutingKey:           cfg.RabbitMQ.RoutingKey,
			DeadLetterExchange:   cfg.RabbitMQ.DeadLetterExchange,
			DeadLetterQueue:      cfg.RabbitMQ.DeadLetterQueue,
			DeadLetterRoutingKey: cfg.RabbitMQ.DeadLetterRoutingKey,
			Prefetch:             cfg.RabbitMQ.Prefetch,
			MaxRetries:           cfg.RabbitMQ.MaxRetries,
			RetryBackoff:         cfg.RabbitMQ.RetryBackoff.String(),
		},
		Testdata: TestdataDTO{
			CacheRoot:         cfg.Testdata.CacheRoot,
			MaxCacheBytes:     cfg.Testdata.MaxCacheBytes,
			MaxUnusedDuration: cfg.Testdata.MaxUnusedDuration.String(),
			CleanupInterval:   cfg.Testdata.CleanupInterval.String(),
			StatsInterval:     cfg.Testdata.StatsInterval.String(),
		},
		GoJudge: GoJudgeDTO{
			Endpoint:            cfg.GoJudge.Endpoint,
			AuthTokenConfigured: cfg.GoJudge.AuthToken != "",
		},
		Reporter: ReporterDTO{Mode: cfg.Reporter.Mode, Endpoint: cfg.Reporter.Endpoint},
		Heartbeat: HeartbeatDTO{
			Enabled:  cfg.Heartbeat.Enabled,
			Endpoint: cfg.Heartbeat.Endpoint,
			Interval: cfg.Heartbeat.Interval.String(),
		},
		Remote: RemoteDTO{Enabled: cfg.Remote.Enabled, Nacos: nacosToDTO(cfg.Remote.Nacos)},
	}
}

func dtoToConfig(dto ConfigDTO, base config.Config) (*config.Config, error) {
	cfg := base
	cfg.Node.Name = dto.Node.Name
	cfg.Node.Type = dto.Node.Type
	cfg.Node.MaxConcurrency = dto.Node.MaxConcurrency
	cfg.Node.SupportedJudgeModes = dto.Node.SupportedJudgeModes
	cfg.HnieOJ.BaseURL = dto.HnieOJ.BaseURL
	var err error
	if cfg.HnieOJ.RequestTimeout, err = parseDuration(dto.HnieOJ.RequestTimeout, 30*time.Second); err != nil {
		return nil, err
	}
	cfg.HnieOJ.FormalToken.CipherAlgorithm = defaultString(dto.HnieOJ.FormalToken.CipherAlgorithm, "RSA/ECB/OAEPWithSHA-256AndMGF1Padding")
	cfg.HnieOJ.FormalToken.Nacos = dtoToNacos(dto.HnieOJ.FormalToken.Nacos)
	if cfg.HnieOJ.FormalToken.RefreshInterval, err = parseDuration(dto.HnieOJ.FormalToken.RefreshInterval, 30*time.Second); err != nil {
		return nil, err
	}
	cfg.RabbitMQ.Host = dto.RabbitMQ.Host
	cfg.RabbitMQ.Port = dto.RabbitMQ.Port
	cfg.RabbitMQ.Username = dto.RabbitMQ.Username
	if dto.RabbitMQ.Password != "" {
		cfg.RabbitMQ.Password = dto.RabbitMQ.Password
	}
	cfg.RabbitMQ.VirtualHost = dto.RabbitMQ.VirtualHost
	cfg.RabbitMQ.Exchange = dto.RabbitMQ.Exchange
	cfg.RabbitMQ.Queue = dto.RabbitMQ.Queue
	cfg.RabbitMQ.RoutingKey = dto.RabbitMQ.RoutingKey
	cfg.RabbitMQ.DeadLetterExchange = dto.RabbitMQ.DeadLetterExchange
	cfg.RabbitMQ.DeadLetterQueue = dto.RabbitMQ.DeadLetterQueue
	cfg.RabbitMQ.DeadLetterRoutingKey = dto.RabbitMQ.DeadLetterRoutingKey
	cfg.RabbitMQ.Prefetch = dto.RabbitMQ.Prefetch
	cfg.RabbitMQ.MaxRetries = dto.RabbitMQ.MaxRetries
	if cfg.RabbitMQ.RetryBackoff, err = parseDuration(dto.RabbitMQ.RetryBackoff, 10*time.Second); err != nil {
		return nil, err
	}
	cfg.Testdata.CacheRoot = dto.Testdata.CacheRoot
	cfg.Testdata.MaxCacheBytes = dto.Testdata.MaxCacheBytes
	if cfg.Testdata.MaxUnusedDuration, err = parseDuration(dto.Testdata.MaxUnusedDuration, 72*time.Hour); err != nil {
		return nil, err
	}
	if cfg.Testdata.CleanupInterval, err = parseDuration(dto.Testdata.CleanupInterval, time.Hour); err != nil {
		return nil, err
	}
	if cfg.Testdata.StatsInterval, err = parseDuration(dto.Testdata.StatsInterval, 5*time.Minute); err != nil {
		return nil, err
	}
	cfg.GoJudge.Endpoint = defaultString(dto.GoJudge.Endpoint, "http://127.0.0.1:5050")
	if dto.GoJudge.AuthToken != "" {
		cfg.GoJudge.AuthToken = dto.GoJudge.AuthToken
	}
	cfg.Reporter.Mode = defaultString(dto.Reporter.Mode, "http")
	cfg.Reporter.Endpoint = defaultString(dto.Reporter.Endpoint, "/judge/submissions/{submissionId}/events")
	cfg.Heartbeat.Enabled = dto.Heartbeat.Enabled
	cfg.Heartbeat.Endpoint = defaultString(dto.Heartbeat.Endpoint, "/judge/nodes/heartbeat")
	if cfg.Heartbeat.Interval, err = parseDuration(dto.Heartbeat.Interval, 30*time.Second); err != nil {
		return nil, err
	}
	cfg.Remote.Enabled = dto.Remote.Enabled
	cfg.Remote.Nacos = dtoToNacos(dto.Remote.Nacos)
	if cfg.HnieOJ.BaseURL == "" {
		return nil, errors.New("hnieoj.baseUrl is required")
	}
	return &cfg, nil
}

func nacosToDTO(n config.NacosConfig) NacosDTO {
	return NacosDTO{ServerAddr: n.ServerAddr, Namespace: n.Namespace, Group: n.Group, DataID: n.DataID}
}

func dtoToNacos(n NacosDTO) config.NacosConfig {
	return config.NacosConfig{ServerAddr: n.ServerAddr, Namespace: n.Namespace, Group: n.Group, DataID: n.DataID}
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
