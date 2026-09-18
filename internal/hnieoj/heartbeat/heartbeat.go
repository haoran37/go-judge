package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
)

const Version = "hnieoj-go-judge-0.2.0"

const defaultCacheStatsInterval = 5 * time.Minute

const maxResponseBytes = 1 << 20

type Client struct {
	cfg          config.Config
	cred         *auth.Credential
	httpClient   *http.Client
	logger       logging.Logger
	running      *atomic.Int64
	draining     *atomic.Bool
	cacheMu      sync.Mutex
	cacheStats   CacheStats
	cacheStatsAt time.Time
}

type Payload struct {
	NodeID              string   `json:"nodeId"`
	NodeName            string   `json:"nodeName"`
	NodeType            string   `json:"nodeType"`
	MaxConcurrency      int      `json:"maxConcurrency"`
	RunningTasks        int64    `json:"runningTasks"`
	CPUCore             int      `json:"cpuCore"`
	Version             string   `json:"version"`
	SupportedJudgeModes []string `json:"supportedJudgeModes"`
	Draining            bool     `json:"draining"`
	CacheUsedBytes      int64    `json:"cacheUsedBytes"`
	CacheProblemCount   int      `json:"cacheProblemCount"`
	DiskTotalBytes      int64    `json:"diskTotalBytes"`
	DiskFreeBytes       int64    `json:"diskFreeBytes"`
}

func New(cfg config.Config, cred *auth.Credential, httpClient *http.Client, logger logging.Logger, running *atomic.Int64, draining *atomic.Bool) *Client {
	if draining == nil {
		draining = &atomic.Bool{}
	}
	return &Client{cfg: cfg, cred: cred, httpClient: httpClient, logger: logger, running: running, draining: draining}
}

func (c *Client) Start(ctx context.Context) {
	if !c.cfg.Heartbeat.Enabled {
		return
	}
	interval := c.cfg.Heartbeat.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			if err := c.Send(ctx); err != nil {
				c.logger.Warn("heartbeat failed", logging.Error(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (c *Client) Send(ctx context.Context) error {
	// 通过 Snapshot 读取凭证，避免与后台续期的 Replace 并发读写。
	cred := c.cred.Snapshot()
	nodeID := cred.NodeID
	if nodeID == "" {
		nodeID = c.cfg.Node.Name
	}
	cacheStats := c.cacheStatsSnapshot()
	body, err := json.Marshal(Payload{
		NodeID:              nodeID,
		NodeName:            c.cfg.Node.Name,
		NodeType:            c.cfg.Node.Type,
		MaxConcurrency:      c.cfg.Node.MaxConcurrency,
		RunningTasks:        c.running.Load(),
		CPUCore:             runtime.NumCPU(),
		Version:             Version,
		SupportedJudgeModes: c.cfg.Node.SupportedJudgeModes,
		Draining:            c.draining.Load(),
		CacheUsedBytes:      cacheStats.CacheUsedBytes,
		CacheProblemCount:   cacheStats.CacheProblemCount,
		DiskTotalBytes:      cacheStats.DiskTotalBytes,
		DiskFreeBytes:       cacheStats.DiskFreeBytes,
	})
	if err != nil {
		return err
	}
	endpoint := c.cfg.Heartbeat.Endpoint
	if endpoint == "" {
		endpoint = "/judge/nodes/heartbeat"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.HnieOJ.BaseURL, "/")+endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.cred.Apply(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > maxResponseBytes {
		return errHeartbeatResponseTooLarge
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &statusError{code: resp.StatusCode}
	}
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return errMalformedHeartbeatResponse
	}
	if envelope.Code != http.StatusOK {
		return &statusError{code: envelope.Code}
	}
	c.logger.Info("heartbeat succeeded")
	return nil
}

func (c *Client) cacheStatsSnapshot() CacheStats {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	interval := c.cfg.Testdata.StatsInterval
	if interval <= 0 {
		interval = defaultCacheStatsInterval
	}
	if !c.cacheStatsAt.IsZero() && time.Since(c.cacheStatsAt) < interval {
		return c.cacheStats
	}
	c.cacheStats = collectCacheStats(c.cfg.Testdata.CacheRoot)
	c.cacheStatsAt = time.Now()
	return c.cacheStats
}

var (
	errHeartbeatResponseTooLarge  = errors.New("heartbeat response body too large")
	errMalformedHeartbeatResponse = errors.New("malformed heartbeat response")
)

type statusError struct {
	code int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("heartbeat status %d", e.code)
}
