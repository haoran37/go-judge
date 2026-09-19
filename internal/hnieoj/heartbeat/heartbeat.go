// Package heartbeat 构造 WSS HEARTBEAT 的 payload（现有指标 + draining）。
// 心跳不再走 HTTP，也不改变服务端批准额度或硬授权。
package heartbeat

import (
	"encoding/json"
	"runtime"
	"sync"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/config"
)

// Version 是节点 agent 版本，随心跳上报。
const Version = "hnieoj-go-judge-0.3.0"

const defaultCacheStatsInterval = 5 * time.Minute

// Payload 是 HEARTBEAT 的业务字段。
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

// Builder 收集节点指标并生成 payload。
type Builder struct {
	cfg          config.Config
	running      atomicInt64
	nodeID       func() string
	cacheMu      sync.Mutex
	cacheStats   CacheStats
	cacheStatsAt time.Time
}

// atomicInt64 收敛对 atomic.Int64 的依赖，便于测试注入。
type atomicInt64 interface {
	Load() int64
}

func NewBuilder(cfg config.Config, running atomicInt64, nodeID func() string) *Builder {
	return &Builder{cfg: cfg, running: running, nodeID: nodeID}
}

// Payload 生成带有 draining 标记的 HEARTBEAT payload。
func (b *Builder) Payload(draining bool) (json.RawMessage, error) {
	nodeID := b.cfg.Node.Name
	if b.nodeID != nil {
		if id := b.nodeID(); id != "" {
			nodeID = id
		}
	}
	var running int64
	if b.running != nil {
		running = b.running.Load()
	}
	cacheStats := b.cacheStatsSnapshot()
	return json.Marshal(Payload{
		NodeID:              nodeID,
		NodeName:            b.cfg.Node.Name,
		NodeType:            b.cfg.Node.Type,
		MaxConcurrency:      b.cfg.Node.MaxConcurrency,
		RunningTasks:        running,
		CPUCore:             runtime.NumCPU(),
		Version:             Version,
		SupportedJudgeModes: b.cfg.Node.SupportedJudgeModes,
		Draining:            draining,
		CacheUsedBytes:      cacheStats.CacheUsedBytes,
		CacheProblemCount:   cacheStats.CacheProblemCount,
		DiskTotalBytes:      cacheStats.DiskTotalBytes,
		DiskFreeBytes:       cacheStats.DiskFreeBytes,
	})
}

func (b *Builder) cacheStatsSnapshot() CacheStats {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	interval := b.cfg.Testdata.StatsInterval
	if interval <= 0 {
		interval = defaultCacheStatsInterval
	}
	if !b.cacheStatsAt.IsZero() && time.Since(b.cacheStatsAt) < interval {
		return b.cacheStats
	}
	b.cacheStats = collectCacheStats(b.cfg.Testdata.CacheRoot)
	b.cacheStatsAt = time.Now()
	return b.cacheStats
}
