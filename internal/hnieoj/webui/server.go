package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/node"
	"github.com/criyle/go-judge/internal/hnieoj/testdata"
)

const sessionCookie = "hnieoj_judge_session"
const sessionTTL = 2 * time.Hour

type Server struct {
	store      *Store
	manager    *node.Manager
	logs       *logging.Recorder
	sessionsMu sync.Mutex
	sessions   map[string]time.Time
}

type ConfigDTO struct {
	Node     NodeDTO     `json:"node"`
	HnieOJ   HnieOJDTO   `json:"hnieoj"`
	Identity IdentityDTO `json:"identity"`
	Rotation RotationDTO `json:"rotation"`
	Testdata TestdataDTO `json:"testdata"`
	GoJudge  GoJudgeDTO  `json:"gojudge"`
	Worker   WorkerDTO   `json:"worker"`
}

type NodeDTO struct {
	Name                string   `json:"name"`
	Type                string   `json:"type"`
	MaxConcurrency      int      `json:"maxConcurrency"`
	SupportedJudgeModes []string `json:"supportedJudgeModes"`
}

type HnieOJDTO struct {
	BaseURL        string `json:"baseUrl"`
	WSSURL         string `json:"wssUrl"`
	Audience       string `json:"audience"`
	RequestTimeout string `json:"requestTimeout"`
}

// IdentityDTO 只暴露配置标志与路径，绝不返回私钥/enrollmentId 之外的秘密。
// bootstrapToken 只写不读。
type IdentityDTO struct {
	File                string `json:"file"`
	StateDir            string `json:"stateDir"`
	BootstrapConfigured bool   `json:"bootstrapConfigured"`
	BootstrapToken      string `json:"bootstrapToken,omitempty"`
}

type RotationDTO struct {
	Enabled        bool   `json:"enabled"`
	Interval       string `json:"interval"`
	Grace          string `json:"grace"`
	ConfirmTimeout string `json:"confirmTimeout"`
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

type WorkerDTO struct {
	EmptyMinBackoff string `json:"emptyMinBackoff"`
	EmptyMaxBackoff string `json:"emptyMaxBackoff"`
	DrainTimeout    string `json:"drainTimeout"`
}

type setupStatusResponse struct {
	AdminInitialized bool        `json:"adminInitialized"`
	Authenticated    bool        `json:"authenticated"`
	Configured       bool        `json:"configured"`
	BootstrapReady   bool        `json:"bootstrapReady"`
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
	mux.HandleFunc("/api/v1/setup/bootstrap", s.withAuth(s.handleSetupBootstrap))
	mux.HandleFunc("/api/v1/runtime/start", s.withAuth(s.handleStart))
	mux.HandleFunc("/api/v1/runtime/stop", s.withAuth(s.handleStop))
	mux.HandleFunc("/api/v1/runtime/restart", s.withAuth(s.handleRestart))
	mux.HandleFunc("/api/v1/status", s.withAuth(s.handleStatus))
	mux.HandleFunc("/api/v1/metrics/summary", s.withAuth(s.handleStatus))
	mux.HandleFunc("/api/v1/system/info", s.withAuth(s.handleSystemInfo))
	mux.HandleFunc("/api/v1/logs/recent", s.withAuth(s.handleLogs))
	mux.HandleFunc("/api/v1/testdata/cache", s.withAuth(s.handleTestdataCache))
	mux.HandleFunc("/api/v1/testdata/cache/cleanup", s.withAuth(s.handleTestdataCacheCleanup))
	mux.HandleFunc("/api/v1/testdata/cache/", s.withAuth(s.handleTestdataCacheItem))
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
		BootstrapReady:   s.store.BootstrapConfigured(),
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
		writeJSON(w, s.configToDTO(*cfg))
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
		cfg := s.dtoToConfig(dto, *current)
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

// handleSetupBootstrap 是统一的正式/临时节点入网入口：保存配置并安全写入一次性
// bootstrap 明文（0600），随后由运行时完成 Ed25519 挑战注册。绝不回显 bootstrap。
func (s *Server) handleSetupBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Config ConfigDTO `json:"config"`
	}
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	base, err := s.configBase()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cfg := s.dtoToConfig(req.Config, base)
	nodeType := strings.TrimSpace(cfg.Node.Type)
	if nodeType != "formal" && nodeType != "temp" {
		http.Error(w, "节点类型必须是 formal 或 temp", http.StatusBadRequest)
		return
	}
	cfg.Bootstrap.TokenFile = s.store.BootstrapPath()
	bootstrapToken := strings.TrimSpace(req.Config.Identity.BootstrapToken)
	cfg.Bootstrap.Token = ""
	if bootstrapToken != "" {
		if err := s.store.WriteBootstrapToken(bootstrapToken); err != nil {
			http.Error(w, "保存 bootstrap 失败："+err.Error(), http.StatusBadRequest)
			return
		}
	} else if !s.store.BootstrapConfigured() {
		http.Error(w, "首次入网需要提供一次性 bootstrap token", http.StatusBadRequest)
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
	writeJSON(w, map[string]any{"ok": true, "config": s.configToDTO(*cfg)})
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

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.effectiveConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, collectSystemInfo(cfg.Testdata.CacheRoot))
}

func (s *Server) handleTestdataCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cfg, err := s.effectiveConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	items, err := testdata.ListCache(cfg.Testdata.CacheRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"items": items})
}

func (s *Server) handleTestdataCacheCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cfg, err := s.effectiveConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result, err := testdata.Cleanup(cfg.Testdata.CacheRoot, cfg.Testdata.MaxCacheBytes, cfg.Testdata.MaxUnusedDuration)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
}

func (s *Server) handleTestdataCacheItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	cfg, err := s.effectiveConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	idText := strings.TrimPrefix(r.URL.Path, "/api/v1/testdata/cache/")
	problemID, err := strconv.ParseInt(strings.TrimSpace(idText), 10, 64)
	if err != nil {
		http.Error(w, "invalid problem id", http.StatusBadRequest)
		return
	}
	if err := testdata.DeleteCachedProblem(cfg.Testdata.CacheRoot, problemID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
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
	expires := time.Now().Add(sessionTTL)
	s.sessionsMu.Lock()
	now := time.Now()
	for existing, existingExpires := range s.sessions {
		if now.After(existingExpires) {
			delete(s.sessions, existing)
		}
	}
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

func (s *Server) configBase() (config.Config, error) {
	current, exists, err := s.store.LoadConfig()
	if err != nil {
		return config.Config{}, err
	}
	if !exists {
		return *config.Default(), nil
	}
	return *current, nil
}

func (s *Server) effectiveConfig() (*config.Config, error) {
	if cfg, ok := s.manager.Config(); ok {
		return cfg, nil
	}
	stored, exists, err := s.store.LoadConfig()
	if err != nil {
		return nil, err
	}
	if exists {
		return stored, nil
	}
	return config.Default(), nil
}

func (s *Server) dtoToConfig(dto ConfigDTO, base config.Config) *config.Config {
	cfg := base
	cfg.Node.Name = defaultString(dto.Node.Name, cfg.Node.Name)
	cfg.Node.Type = defaultString(dto.Node.Type, cfg.Node.Type)
	cfg.Node.MaxConcurrency = dto.Node.MaxConcurrency
	cfg.Node.SupportedJudgeModes = dto.Node.SupportedJudgeModes
	cfg.HnieOJ.BaseURL = dto.HnieOJ.BaseURL
	cfg.HnieOJ.WSSURL = dto.HnieOJ.WSSURL
	cfg.HnieOJ.Audience = dto.HnieOJ.Audience
	cfg.HnieOJ.RequestTimeout = parseDurationOrDefault(dto.HnieOJ.RequestTimeout, cfg.HnieOJ.RequestTimeout, 30*time.Second)
	if strings.TrimSpace(dto.Identity.File) != "" {
		cfg.Identity.File = dto.Identity.File
	}
	if strings.TrimSpace(cfg.Identity.File) == "" {
		cfg.Identity.File = s.store.IdentityPath()
	}
	if strings.TrimSpace(dto.Identity.StateDir) != "" {
		cfg.Identity.StateDir = dto.Identity.StateDir
	}
	if strings.TrimSpace(cfg.Identity.StateDir) == "" {
		cfg.Identity.StateDir = s.store.Dir()
	}
	if strings.TrimSpace(cfg.WSS.ResultQueueDir) == "" {
		cfg.WSS.ResultQueueDir = filepath.Join(cfg.Identity.StateDir, "results")
	}
	cfg.Rotation.Enabled = dto.Rotation.Enabled
	cfg.Rotation.Interval = parseDurationOrDefault(dto.Rotation.Interval, cfg.Rotation.Interval, 30*24*time.Hour)
	cfg.Rotation.Grace = parseDurationOrDefault(dto.Rotation.Grace, cfg.Rotation.Grace, 5*time.Minute)
	cfg.Rotation.ConfirmTimeout = parseDurationOrDefault(dto.Rotation.ConfirmTimeout, cfg.Rotation.ConfirmTimeout, 30*time.Second)
	cfg.Testdata.CacheRoot = defaultString(dto.Testdata.CacheRoot, cfg.Testdata.CacheRoot)
	cfg.Testdata.MaxCacheBytes = dto.Testdata.MaxCacheBytes
	cfg.Testdata.MaxUnusedDuration = parseDurationOrDefault(dto.Testdata.MaxUnusedDuration, cfg.Testdata.MaxUnusedDuration, 72*time.Hour)
	cfg.Testdata.CleanupInterval = parseDurationOrDefault(dto.Testdata.CleanupInterval, cfg.Testdata.CleanupInterval, time.Hour)
	cfg.Testdata.StatsInterval = parseDurationOrDefault(dto.Testdata.StatsInterval, cfg.Testdata.StatsInterval, 5*time.Minute)
	cfg.GoJudge.Endpoint = defaultString(dto.GoJudge.Endpoint, "http://127.0.0.1:5050")
	if dto.GoJudge.AuthToken != "" {
		cfg.GoJudge.AuthToken = dto.GoJudge.AuthToken
	}
	cfg.Worker.EmptyMinBackoff = parseDurationOrDefault(dto.Worker.EmptyMinBackoff, cfg.Worker.EmptyMinBackoff, 200*time.Millisecond)
	cfg.Worker.EmptyMaxBackoff = parseDurationOrDefault(dto.Worker.EmptyMaxBackoff, cfg.Worker.EmptyMaxBackoff, 5*time.Second)
	cfg.Worker.DrainTimeout = parseDurationOrDefault(dto.Worker.DrainTimeout, cfg.Worker.DrainTimeout, 5*time.Minute)
	return &cfg
}

func (s *Server) configToDTO(cfg config.Config) ConfigDTO {
	return ConfigDTO{
		Node: NodeDTO{
			Name:                cfg.Node.Name,
			Type:                cfg.Node.Type,
			MaxConcurrency:      cfg.Node.MaxConcurrency,
			SupportedJudgeModes: cfg.Node.SupportedJudgeModes,
		},
		HnieOJ: HnieOJDTO{
			BaseURL:        cfg.HnieOJ.BaseURL,
			WSSURL:         cfg.HnieOJ.WSSURL,
			Audience:       cfg.HnieOJ.Audience,
			RequestTimeout: cfg.HnieOJ.RequestTimeout.String(),
		},
		Identity: IdentityDTO{
			File:                cfg.Identity.File,
			StateDir:            cfg.Identity.StateDir,
			BootstrapConfigured: s.store.BootstrapConfigured(),
		},
		Rotation: RotationDTO{
			Enabled:        cfg.Rotation.Enabled,
			Interval:       cfg.Rotation.Interval.String(),
			Grace:          cfg.Rotation.Grace.String(),
			ConfirmTimeout: cfg.Rotation.ConfirmTimeout.String(),
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
		Worker: WorkerDTO{
			EmptyMinBackoff: cfg.Worker.EmptyMinBackoff.String(),
			EmptyMaxBackoff: cfg.Worker.EmptyMaxBackoff.String(),
			DrainTimeout:    cfg.Worker.DrainTimeout.String(),
		},
	}
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

func parseDurationOrDefault(value string, fallback, def time.Duration) time.Duration {
	if strings.TrimSpace(value) == "" {
		if fallback > 0 {
			return fallback
		}
		return def
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		if fallback > 0 {
			return fallback
		}
		return def
	}
	return parsed
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
