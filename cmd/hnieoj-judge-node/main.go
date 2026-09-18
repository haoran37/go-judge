package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/gateway"
	"github.com/criyle/go-judge/internal/hnieoj/heartbeat"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/processor"
	"github.com/criyle/go-judge/internal/hnieoj/reporter"
	"github.com/criyle/go-judge/internal/hnieoj/runner"
	"github.com/criyle/go-judge/internal/hnieoj/testdata"
	"github.com/criyle/go-judge/internal/hnieoj/worker"
	"go.uber.org/zap"
)

// finalDrainHeartbeatTimeout 限制关闭时最后一跳 draining 心跳的等待时间，
// 保证空闲/立即退出时 shutdown 不会被网络请求挂住。
const finalDrainHeartbeatTimeout = 5 * time.Second

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	cfg, fixturePath, err := config.LoadFromArgs()
	if err != nil {
		logger.Fatal("config load failed", zap.Error(err))
	}

	// signalCtx 只负责停止领取新任务；auxCtx 让凭证续期、心跳与缓存清理
	// 存活到在途任务排空（或排空超时）之后再退出。
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	auxCtx, cancelAux := context.WithCancel(context.Background())
	defer cancelAux()

	appLogger := zapAdapter{logger: logger}
	httpClient := &http.Client{Timeout: cfg.HnieOJ.RequestTimeout}
	credManager, err := auth.Load(signalCtx, *cfg, httpClient, appLogger)
	if err != nil {
		logger.Fatal("credential load failed", zap.Error(err))
	}
	cred := credManager.Credential()
	credManager.StartRenewal(auxCtx)
	initial := cred.Snapshot()
	logger.Info("credential ready", zap.String("nodeType", cfg.Node.Type), zap.String("nodeName", cfg.Node.Name), zap.String("nodeId", initial.NodeID), zap.String("tokenId", initial.TokenID))

	rep := buildReporter(*cfg, httpClient, cred, appLogger)
	testdataClient := testdata.New(cfg.HnieOJ.BaseURL, cfg.Testdata.CacheRoot, httpClient, cred, appLogger)
	testdataClient.StartCleaner(auxCtx, cfg.Testdata.CleanupInterval, cfg.Testdata.MaxCacheBytes, cfg.Testdata.MaxUnusedDuration)
	runnerClient := runner.New(cfg.GoJudge.Endpoint, cfg.GoJudge.AuthToken, httpClient, appLogger)
	proc := processor.New(testdataClient, runnerClient, rep, cred, appLogger, cfg.Node.SupportedJudgeModes)

	if fixturePath != "" {
		// fixture 仅用于本地/测试排查，强制日志上报，禁止把任意 fixture 结果写入真实后端。
		if cfg.Reporter.Mode != "log" {
			logger.Fatal("fixture mode requires reporter.mode=log; live backend reporting is not allowed")
		}
		if err := runFixture(signalCtx, fixturePath, proc.Process); err != nil {
			logger.Fatal("fixture failed", zap.Error(err))
		}
		return
	}

	gatewayClient := gateway.New(cfg.HnieOJ.BaseURL, httpClient, cred, appLogger)
	pool := worker.New(gatewayClient, gatewayClient, proc.Process, appLogger, worker.Options{
		Slots:             cfg.Node.MaxConcurrency,
		EmptyMinBackoff:   cfg.Worker.EmptyMinBackoff,
		EmptyMaxBackoff:   cfg.Worker.EmptyMaxBackoff,
		DrainTimeout:      cfg.Worker.DrainTimeout,
		RenewRetryBackoff: cfg.HnieOJ.Renew.RetryBackoff,
	})

	draining := &atomic.Bool{}
	heartbeatClient := heartbeat.New(*cfg, cred, httpClient, appLogger, pool.Active(), draining)
	heartbeatClient.Start(auxCtx)

	logger.Info("node started",
		zap.String("nodeName", cfg.Node.Name),
		zap.String("nodeType", cfg.Node.Type),
		zap.Int("maxConcurrency", cfg.Node.MaxConcurrency),
		zap.Strings("supportedJudgeModes", cfg.Node.SupportedJudgeModes))
	if err := coordinateShutdown(signalCtx, cancelAux, heartbeatClient, draining, pool, cfg.Heartbeat.Enabled, appLogger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Fatal("worker pool stopped", zap.Error(err))
	}
	logger.Info("node stopped")
}

// coordinateShutdown 在收到关闭信号后立即停止领取新任务，并让凭证续期与心跳
// 继续存活到在途任务排空（或排空超时）。退出前显式发布 draining=true 并发送
// 一跳带超时的最终心跳；随后才取消辅助协程，确保不会留下后台泄漏。
func coordinateShutdown(signalCtx context.Context, cancelAux context.CancelFunc, heartbeatClient *heartbeat.Client, draining *atomic.Bool, pool *worker.Pool, heartbeatEnabled bool, logger logging.Logger) error {
	var auxWG sync.WaitGroup
	auxWG.Add(1)
	go func() {
		defer auxWG.Done()
		<-signalCtx.Done()
		draining.Store(true)
		if !heartbeatEnabled {
			return
		}
		// 用独立的受限 context 发送，避免心跳在 signalCtx 取消后不再发出。
		ctx, cancel := context.WithTimeout(context.Background(), finalDrainHeartbeatTimeout)
		defer cancel()
		if err := heartbeatClient.Send(ctx); err != nil {
			logger.Warn("final draining heartbeat failed", logging.Error(err))
		}
	}()

	// pool.Run 在 signalCtx 取消后停止领取，并在有界窗口内排空在途任务。
	err := pool.Run(signalCtx)
	cancelAux()
	auxWG.Wait()
	return err
}

func buildReporter(cfg config.Config, httpClient *http.Client, cred *auth.Credential, logger logging.Logger) reporter.Reporter {
	if cfg.Reporter.Mode == "log" || cfg.Reporter.Mode == "mock" {
		return reporter.NewLog(logger)
	}
	return reporter.NewHTTP(cfg.HnieOJ.BaseURL, cfg.Reporter.Endpoint, httpClient, cred, logger, cfg.Reporter.MaxRetries, cfg.Reporter.RetryBackoff)
}

func runFixture(ctx context.Context, path string, handler func(context.Context, model.Task) error) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var task model.Task
	if err := json.Unmarshal(b, &task); err != nil {
		return err
	}
	return handler(ctx, task)
}
