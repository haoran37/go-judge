package node

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/heartbeat"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/mq"
	"github.com/criyle/go-judge/internal/hnieoj/processor"
	"github.com/criyle/go-judge/internal/hnieoj/reporter"
	"github.com/criyle/go-judge/internal/hnieoj/runner"
	"github.com/criyle/go-judge/internal/hnieoj/testdata"
)

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
	State         State          `json:"state"`
	Configured    bool           `json:"configured"`
	NodeName      string         `json:"nodeName"`
	NodeType      string         `json:"nodeType"`
	RunningTasks  int64          `json:"runningTasks"`
	StartedAt     *time.Time     `json:"startedAt,omitempty"`
	StoppedAt     *time.Time     `json:"stoppedAt,omitempty"`
	LastError     string         `json:"lastError,omitempty"`
	Metrics       Metrics        `json:"metrics"`
	RecentMetrics []MetricBucket `json:"recentMetrics"`
}

type Manager struct {
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
}

func NewManager(logger logging.Logger) *Manager {
	return &Manager{logger: logger, state: StateStopped, metricBuckets: map[int64]*MetricBucket{}}
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
	m.mu.Lock()
	if m.state == StateRunning || m.state == StateStarting {
		m.mu.Unlock()
		return nil
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
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.cancel = cancel
	m.done = done
	m.started = time.Now()
	m.stopped = time.Time{}
	m.mu.Unlock()

	if err := cfg.Validate(); err != nil {
		cancel()
		m.fail(err)
		close(done)
		return err
	}
	if err := m.startSandbox(runCtx); err != nil {
		cancel()
		m.fail(err)
		close(done)
		return err
	}

	go m.run(runCtx, cfg, done)
	m.mu.Lock()
	m.state = StateRunning
	m.mu.Unlock()
	return nil
}

func (m *Manager) Stop(ctx context.Context) error {
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
	m.stopSandbox()
	m.mu.Lock()
	m.state = StateStopped
	m.stopped = time.Now()
	m.mu.Unlock()
	return nil
}

func (m *Manager) Restart(ctx context.Context) error {
	if err := m.Stop(ctx); err != nil {
		return err
	}
	return m.Start(ctx)
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

func (m *Manager) run(ctx context.Context, cfg config.Config, done chan struct{}) {
	defer close(done)
	httpClient := &http.Client{Timeout: cfg.HnieOJ.RequestTimeout}
	cred, err := auth.Authenticate(ctx, cfg, httpClient)
	if err != nil {
		m.fail(err)
		return
	}
	m.logger.Info("auth succeeded", logging.String("nodeType", cfg.Node.Type), logging.String("nodeName", cfg.Node.Name))

	rep := buildReporter(cfg, httpClient, cred, m.logger)
	testdataClient := testdata.New(cfg.HnieOJ.BaseURL, cfg.Testdata.CacheRoot, httpClient, cred, m.logger)
	testdataClient.StartCleaner(ctx, cfg.Testdata.CleanupInterval, cfg.Testdata.MaxCacheBytes, cfg.Testdata.MaxUnusedDuration)
	runnerClient := runner.New(cfg.GoJudge.Endpoint, cfg.GoJudge.AuthToken, httpClient, m.logger)
	proc := processor.New(testdataClient, runnerClient, rep, cred, m.logger, cfg.Node.SupportedJudgeModes)
	heartbeat.New(cfg, cred, httpClient, m.logger, &m.running).Start(ctx)

	handler := limitedHandler(cfg.Node.MaxConcurrency, &m.running, m.wrapProcess(proc.Process))
	consumer := mq.New(cfg.RabbitMQ, m.logger)
	if err := consumer.Consume(ctx, handler); err != nil && !errors.Is(err, context.Canceled) {
		m.fail(err)
		return
	}
}

func (m *Manager) wrapProcess(handler func(context.Context, model.Task) error) func(context.Context, model.Task) error {
	return func(ctx context.Context, task model.Task) error {
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

func (m *Manager) startSandbox(ctx context.Context) error {
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

func buildReporter(cfg config.Config, httpClient *http.Client, cred *auth.Credential, logger logging.Logger) reporter.Reporter {
	if cfg.Reporter.Mode == "log" || cfg.Reporter.Mode == "mock" {
		return reporter.NewLog(logger)
	}
	return reporter.NewHTTP(cfg.HnieOJ.BaseURL, cfg.Reporter.Endpoint, httpClient, cred, logger)
}

func limitedHandler(maxConcurrency int, running *atomic.Int64, handler func(context.Context, model.Task) error) func(context.Context, model.Task) error {
	sem := make(chan struct{}, maxConcurrency)
	return func(ctx context.Context, task model.Task) error {
		select {
		case sem <- struct{}{}:
			running.Add(1)
			defer func() {
				running.Add(-1)
				<-sem
			}()
			return handler(ctx, task)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
