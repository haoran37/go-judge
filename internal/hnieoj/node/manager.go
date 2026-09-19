package node

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/enrollment"
	"github.com/criyle/go-judge/internal/hnieoj/heartbeat"
	"github.com/criyle/go-judge/internal/hnieoj/httpsign"
	"github.com/criyle/go-judge/internal/hnieoj/identity"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/processor"
	"github.com/criyle/go-judge/internal/hnieoj/resultqueue"
	"github.com/criyle/go-judge/internal/hnieoj/rotation"
	"github.com/criyle/go-judge/internal/hnieoj/runner"
	"github.com/criyle/go-judge/internal/hnieoj/tasks"
	"github.com/criyle/go-judge/internal/hnieoj/testdata"
	"github.com/criyle/go-judge/internal/hnieoj/worker"
	"github.com/criyle/go-judge/internal/hnieoj/wss"
)

// drainFlushInterval 是排空收尾时尝试重发结果的节奏。
const drainFlushInterval = 500 * time.Millisecond

type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateFailed   State = "failed"
)

type Metrics struct {
	StartedTasks   int64 `json:"startedTasks"`
	FinishedTasks  int64 `json:"finishedTasks"`
	FailedTasks    int64 `json:"failedTasks"`
	RetryableTasks int64 `json:"retryableTasks"`
}

type MetricBucket struct {
	Time           time.Time `json:"time"`
	StartedTasks   int64     `json:"startedTasks"`
	FinishedTasks  int64     `json:"finishedTasks"`
	FailedTasks    int64     `json:"failedTasks"`
	RetryableTasks int64     `json:"retryableTasks"`
}

type Status struct {
	State          State          `json:"state"`
	Configured     bool           `json:"configured"`
	NodeName       string         `json:"nodeName"`
	NodeType       string         `json:"nodeType"`
	NodeID         string         `json:"nodeId,omitempty"`
	KeyID          string         `json:"keyId,omitempty"`
	SessionEpoch   int64          `json:"sessionEpoch,omitempty"`
	Draining       bool           `json:"draining"`
	PendingResults int            `json:"pendingResults"`
	RunningTasks   int64          `json:"runningTasks"`
	StartedAt      *time.Time     `json:"startedAt,omitempty"`
	StoppedAt      *time.Time     `json:"stoppedAt,omitempty"`
	LastError      string         `json:"lastError,omitempty"`
	Metrics        Metrics        `json:"metrics"`
	RecentMetrics  []MetricBucket `json:"recentMetrics"`
	// IdentityPublic 只包含 public 元数据，不含私钥/bootstrap/token。
	IdentityPublic *identity.Public `json:"identity,omitempty"`
}

type Manager struct {
	// opMu 串行化 Start/Stop/Restart，避免生命周期操作重叠或竞态。
	opMu          sync.Mutex
	mu            sync.Mutex
	cfg           *config.Config
	logger        logging.Logger
	state         State
	cancel        context.CancelFunc
	done          chan struct{}
	started       time.Time
	stopped       time.Time
	lastErr       string
	running       atomic.Int64
	metrics       Metrics
	metricBuckets map[int64]*MetricBucket
	sandbox       *exec.Cmd
	// 运行期只读引用，供 Status 展示。
	identityStore *identity.Store
	wssClient     *wss.Client
	queue         *resultqueue.Queue
	// startSandbox 与 processTask 是测试注入点，生产路径使用默认实现。
	startSandbox func(context.Context) error
	processTask  func(context.Context, model.Task) error
}

func NewManager(logger logging.Logger) *Manager {
	manager := &Manager{logger: logger, state: StateStopped, metricBuckets: map[int64]*MetricBucket{}}
	manager.startSandbox = manager.startSandboxProcess
	return manager
}

func (m *Manager) SetConfig(cfg config.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := cfg
	m.cfg = &cp
}

func (m *Manager) Config() (*config.Config, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg == nil {
		return nil, false
	}
	cp := *m.cfg
	return &cp, true
}

func (m *Manager) Start(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.start(ctx)
}

func (m *Manager) start(ctx context.Context) error {
	m.mu.Lock()
	if m.state == StateRunning || m.state == StateStarting {
		m.mu.Unlock()
		return nil
	}
	if m.state == StateStopping {
		m.mu.Unlock()
		return errors.New("judge runtime is still stopping")
	}
	if m.cfg == nil {
		m.state = StateFailed
		m.lastErr = "judge node is not configured"
		m.mu.Unlock()
		return errors.New(m.lastErr)
	}
	cfg := *m.cfg
	m.state = StateStarting
	m.lastErr = ""
	// runCtx 只负责停止领取新任务；auxCtx 让沙箱、WSS 会话、租约续期与轮换存活到排空结束。
	runCtx, cancel := context.WithCancel(context.Background())
	auxCtx, cancelAux := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.cancel = cancel
	m.done = done
	m.started = time.Now()
	m.stopped = time.Time{}
	m.mu.Unlock()

	if err := cfg.Validate(); err != nil {
		cancel()
		cancelAux()
		m.fail(err)
		close(done)
		return err
	}
	if err := m.startSandbox(auxCtx); err != nil {
		cancel()
		cancelAux()
		m.fail(err)
		close(done)
		return err
	}

	m.mu.Lock()
	m.state = StateRunning
	m.mu.Unlock()
	go m.run(runCtx, auxCtx, cancel, cancelAux, cfg, done)
	return nil
}

func (m *Manager) Stop(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.stop(ctx)
}

func (m *Manager) stop(ctx context.Context) error {
	m.mu.Lock()
	if m.state == StateStopped {
		m.mu.Unlock()
		return nil
	}
	cancel := m.cancel
	done := m.done
	m.state = StateStopping
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// 在途任务已排空、辅助 goroutine 已退出后才杀掉沙箱。
	m.stopSandbox()
	m.mu.Lock()
	m.state = StateStopped
	m.stopped = time.Now()
	m.mu.Unlock()
	return nil
}

func (m *Manager) Restart(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.stop(ctx); err != nil {
		return err
	}
	return m.start(ctx)
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := Status{
		State:         m.state,
		Configured:    m.cfg != nil,
		RunningTasks:  m.running.Load(),
		LastError:     m.lastErr,
		Metrics:       m.metrics,
		RecentMetrics: m.recentMetricsLocked(),
	}
	if m.cfg != nil {
		status.NodeName = m.cfg.Node.Name
		status.NodeType = m.cfg.Node.Type
	}
	if m.identityStore != nil {
		pub := m.identityStore.Public()
		status.IdentityPublic = &pub
		status.NodeID = pub.NodeID
		status.KeyID = pub.KeyID
	}
	if m.wssClient != nil {
		status.SessionEpoch = m.wssClient.Epoch()
		status.Draining = m.wssClient.Draining()
	}
	if m.queue != nil {
		status.PendingResults = m.queue.Len()
	}
	if !m.started.IsZero() {
		started := m.started
		status.StartedAt = &started
	}
	if !m.stopped.IsZero() {
		stopped := m.stopped
		status.StoppedAt = &stopped
	}
	return status
}

// run 是节点运行主体：加载身份并完成入网、建立 WSS 任务通道、执行有界任务，
// 停止时先停止领取并发送 NODE_DRAIN，等有界排空结束后才取消辅助 goroutine。
func (m *Manager) run(runCtx, auxCtx context.Context, cancelRun, cancelAux context.CancelFunc, cfg config.Config, done chan struct{}) {
	defer close(done)
	defer cancelAux()

	store, err := identity.Open(cfg.Identity.File, cfg.Node.Name, cfg.Node.Type)
	if err != nil {
		m.fail(err)
		return
	}
	m.mu.Lock()
	m.identityStore = store
	m.mu.Unlock()

	httpClient := &http.Client{Timeout: cfg.HnieOJ.RequestTimeout}
	enrollClient := enrollment.New(
		cfg.HnieOJ.BaseURL, cfg.Node.Name, store,
		enrollment.Bootstrap{TokenFile: cfg.Bootstrap.TokenFile, Token: cfg.Bootstrap.Token},
		httpClient, m.logger,
	)
	// 入网在任何任务存在之前完成，必须能被 Stop 立即取消（runCtx），
	// 不能绑定到只在 run 返回时才取消的 auxCtx。
	if err := enrollClient.EnsureEnrolled(runCtx, cfg.WSS.ConnectMinBackoff); err != nil {
		m.fail(err)
		return
	}

	cred := &auth.Credential{Audience: audienceOf(cfg)}
	audience := audienceOf(cfg)
	signer := httpsign.New(store, cred, audience, httpClient)
	registry := tasks.NewRegistry()
	queue, err := resultqueue.Open(cfg.WSS.ResultQueueDir, cfg.WSS.ResultMaxRecords, cfg.WSS.ResultMaxBytes, cfg.WSS.ResultMaxRecordBytes, cfg.WSS.ResultTTL)
	if err != nil {
		m.fail(err)
		return
	}
	m.mu.Lock()
	m.queue = queue
	m.mu.Unlock()

	var heartbeatPayload func(bool) (json.RawMessage, error)
	revoked := &atomic.Bool{}
	wssClient := wss.New(store, cred, registry, queue, m.logger, wss.Options{
		URL:                 cfg.HnieOJ.WSSURL,
		Audience:            audience,
		NodeName:            cfg.Node.Name,
		NodeType:            cfg.Node.Type,
		LocalConcurrency:    cfg.Node.MaxConcurrency,
		SupportedJudgeModes: cfg.Node.SupportedJudgeModes,
		ControlFrameBytes:   cfg.WSS.ControlFrameBytes,
		TaskFrameBytes:      cfg.WSS.TaskFrameBytes,
		AuthDeadline:        cfg.WSS.AuthDeadline,
		RequestTimeout:      cfg.WSS.RequestTimeout,
		ConnectMinBackoff:   cfg.WSS.ConnectMinBackoff,
		ConnectMaxBackoff:   cfg.WSS.ConnectMaxBackoff,
		HeartbeatFallback:   cfg.WSS.HeartbeatInterval,
		ResultRetryBackoff:  cfg.WSS.ResultRetryBackoff,
		ResultTTL:           cfg.WSS.ResultTTL,
		WriteTimeout:        cfg.WSS.WriteTimeout,
		HeartbeatPayload: func(draining bool) (json.RawMessage, error) {
			if heartbeatPayload == nil {
				return json.Marshal(map[string]any{"draining": draining})
			}
			return heartbeatPayload(draining)
		},
		OnRevoked: func(reason string) {
			// 管理员撤销/禁用/到期：取消在途任务，停止运行时，不自动重新注册绕过。
			revoked.Store(true)
			registry.CancelAll()
			cancelRun()
			m.logger.Warn("node identity revoked", logging.String("reason", reason))
		},
	})
	m.mu.Lock()
	m.wssClient = wssClient
	m.mu.Unlock()
	// 签名 HTTPS 的时间戳也使用服务端校正时间，避免本地时钟漂移导致签名被拒。
	signer.SetNow(wssClient.ServerNow)
	// 身份 GRACE 授权截止同样以服务端校正时间为准，与 WSS/签名路径共用一个时间源。
	store.SetNow(wssClient.ServerNow)

	var handler func(context.Context, model.Task) error
	if m.processTask != nil {
		handler = m.processTask
	} else {
		testdataClient := testdata.New(cfg.HnieOJ.BaseURL, cfg.Testdata.CacheRoot, signer, m.logger)
		testdataClient.StartCleaner(auxCtx, cfg.Testdata.CleanupInterval, cfg.Testdata.MaxCacheBytes, cfg.Testdata.MaxUnusedDuration)
		runnerClient := runner.New(cfg.GoJudge.Endpoint, cfg.GoJudge.AuthToken, httpClient, m.logger)
		proc := processor.New(testdataClient, runnerClient, wssClient, cred, m.logger, cfg.Node.SupportedJudgeModes)
		// 授权到期判断复用 WSS 服务端校正时间，本地时钟领先/落后不误判合法 token。
		proc.SetNow(wssClient.ServerNow)
		handler = proc.Process
	}

	pool := worker.New(wssClient, wssClient, m.wrapProcess(handler), m.logger, worker.Options{
		Slots:             cfg.Node.MaxConcurrency,
		EmptyMinBackoff:   cfg.Worker.EmptyMinBackoff,
		EmptyMaxBackoff:   cfg.Worker.EmptyMaxBackoff,
		DrainTimeout:      cfg.Worker.DrainTimeout,
		RenewRetryBackoff: cfg.WSS.ResultRetryBackoff,
		Registry:          registry,
		Now:               wssClient.ServerNow,
	})
	hbBuilder := heartbeat.NewBuilder(cfg, pool.Active(), store.NodeID)
	heartbeatPayload = hbBuilder.Payload

	// 自动密钥轮换：默认 30 天，确认成功后触发 AUTH_REFRESH 绑定新 key。
	rotator := rotation.New(store, signer, cred, cfg.HnieOJ.BaseURL, audience, m.logger, rotation.Options{
		Enabled:        cfg.Rotation.Enabled,
		Interval:       cfg.Rotation.Interval,
		Grace:          cfg.Rotation.Grace,
		ConfirmTimeout: cfg.Rotation.ConfirmTimeout,
		Now:            wssClient.ServerNow,
	}, wssClient.TriggerRefresh)

	var wssErr atomic.Value
	var wssWG sync.WaitGroup
	wssWG.Add(1)
	go func() {
		defer wssWG.Done()
		if err := wssClient.Run(auxCtx); err != nil && auxCtx.Err() == nil {
			wssErr.Store(err)
			// 任务通道永久失败：停止领取并进入收尾，状态最终收敛为 failed。
			cancelRun()
		}
	}()
	go rotator.Run(auxCtx)

	// 停止信号：立即停止新 READY 与分配、发送 NODE_DRAIN；会话保持存活以续租/发结果。
	go func() {
		<-runCtx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := wssClient.BeginDrain(ctx); err != nil {
			m.logger.Warn("send NODE_DRAIN failed", logging.Error(err))
		}
	}()

	// pool.Run 在 runCtx 取消后停止领取，并在有界窗口内排空在途任务。
	err = pool.Run(runCtx)
	m.flushResults(&cfg, wssClient)
	cancelAux()
	wssWG.Wait()
	registry.CancelAll()

	if err != nil && !errors.Is(err, context.Canceled) {
		m.fail(err)
		return
	}
	if v := wssErr.Load(); v != nil {
		m.fail(v.(error))
		return
	}
	if revoked.Load() {
		m.fail(errors.New("node identity revoked by server"))
		return
	}
	m.mu.Lock()
	if m.state == StateStopping {
		m.state = StateStopped
		m.stopped = time.Now()
	}
	m.mu.Unlock()
}

// flushResults 在排空收尾时有界地重发尚未确认的终态结果。
func (m *Manager) flushResults(cfg *config.Config, client *wss.Client) {
	deadline := time.Now().Add(cfg.Worker.DrainTimeout)
	for client.QueueLen() > 0 && time.Now().Before(deadline) {
		if !client.HasSession() {
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		client.FlushResults(ctx)
		cancel()
		if client.QueueLen() == 0 {
			return
		}
		time.Sleep(drainFlushInterval)
	}
	if client.QueueLen() > 0 {
		m.logger.Warn("result queue not fully acknowledged before shutdown", logging.Int("pending", client.QueueLen()))
	}
}

func (m *Manager) wrapProcess(handler func(context.Context, model.Task) error) func(context.Context, model.Task) error {
	return func(ctx context.Context, task model.Task) error {
		m.running.Add(1)
		defer m.running.Add(-1)
		m.recordMetric(func(metrics *Metrics) { metrics.StartedTasks++ })
		err := handler(ctx, task)
		if err != nil {
			m.recordMetric(func(metrics *Metrics) {
				metrics.FailedTasks++
				var retryable processor.ErrRetryable
				if errors.As(err, &retryable) {
					metrics.RetryableTasks++
				}
			})
			return err
		}
		m.recordMetric(func(metrics *Metrics) { metrics.FinishedTasks++ })
		return nil
	}
}

func (m *Manager) recordMetric(update func(*Metrics)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	update(&m.metrics)
	now := time.Now()
	bucket := m.currentMetricBucketLocked(now)
	bucketMetrics := Metrics{
		StartedTasks:   bucket.StartedTasks,
		FinishedTasks:  bucket.FinishedTasks,
		FailedTasks:    bucket.FailedTasks,
		RetryableTasks: bucket.RetryableTasks,
	}
	update(&bucketMetrics)
	bucket.StartedTasks = bucketMetrics.StartedTasks
	bucket.FinishedTasks = bucketMetrics.FinishedTasks
	bucket.FailedTasks = bucketMetrics.FailedTasks
	bucket.RetryableTasks = bucketMetrics.RetryableTasks
	m.pruneMetricBucketsLocked(now)
}

func (m *Manager) currentMetricBucketLocked(now time.Time) *MetricBucket {
	if m.metricBuckets == nil {
		m.metricBuckets = map[int64]*MetricBucket{}
	}
	minute := now.UTC().Truncate(time.Minute)
	key := minute.Unix()
	bucket := m.metricBuckets[key]
	if bucket == nil {
		bucket = &MetricBucket{Time: minute}
		m.metricBuckets[key] = bucket
	}
	return bucket
}

func (m *Manager) recentMetricsLocked() []MetricBucket {
	now := time.Now()
	m.pruneMetricBucketsLocked(now)
	start := now.UTC().Truncate(time.Minute).Add(-29 * time.Minute)
	out := make([]MetricBucket, 0, 30)
	for i := 0; i < 30; i++ {
		t := start.Add(time.Duration(i) * time.Minute)
		if bucket := m.metricBuckets[t.Unix()]; bucket != nil {
			out = append(out, *bucket)
			continue
		}
		out = append(out, MetricBucket{Time: t})
	}
	return out
}

func (m *Manager) pruneMetricBucketsLocked(now time.Time) {
	cutoff := now.UTC().Truncate(time.Minute).Add(-30 * time.Minute).Unix()
	for key := range m.metricBuckets {
		if key < cutoff {
			delete(m.metricBuckets, key)
		}
	}
}

func (m *Manager) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = StateFailed
	m.lastErr = err.Error()
	m.stopped = time.Now()
	m.logger.Warn("judge runtime failed", logging.Error(err))
}

func (m *Manager) startSandboxProcess(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "/usr/local/bin/go-judge",
		"-http-addr=127.0.0.1:5050",
		"-mount-conf=/opt/go-judge/mount.yaml",
		"-file-timeout=30m",
	)
	if err := cmd.Start(); err != nil {
		return err
	}
	m.mu.Lock()
	m.sandbox = cmd
	m.mu.Unlock()
	go func() {
		err := cmd.Wait()
		if err != nil && ctx.Err() == nil {
			m.logger.Warn("go-judge sandbox exited", logging.Error(err))
		}
	}()
	time.Sleep(500 * time.Millisecond)
	return nil
}

func (m *Manager) stopSandbox() {
	m.mu.Lock()
	cmd := m.sandbox
	m.sandbox = nil
	m.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func audienceOf(cfg config.Config) string {
	if audience := cfg.HnieOJ.Audience; audience != "" {
		return audience
	}
	if u, err := url.Parse(cfg.HnieOJ.BaseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return cfg.HnieOJ.BaseURL
}
