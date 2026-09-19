package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/node"
	"github.com/criyle/go-judge/internal/hnieoj/webui"
	"go.uber.org/zap"
)

const (
	// defaultWebAddr 默认只绑定 loopback，避免把本地管理界面暴露到公网。
	defaultWebAddr  = "127.0.0.1:3723"
	defaultStateDir = "/var/lib/hnieoj-judge-node"
	// minShutdownTimeout 是关闭 WebUI 与运行时的下限。
	minShutdownTimeout = 90 * time.Second
	// shutdownMargin 预留给 WebUI 关闭与进程收尾。
	shutdownMargin = 15 * time.Second
)

// shutdownTimeoutFor 依据配置的排空窗口计算关闭超时：
// pool 排空 + flushResults 各有一段 DrainTimeout，固定 90s 会在默认 5min 排空前提前杀进程。
func shutdownTimeoutFor(cfg *config.Config) time.Duration {
	timeout := minShutdownTimeout
	if cfg == nil {
		return timeout
	}
	drain := cfg.Worker.DrainTimeout
	if drain <= 0 {
		drain = 5 * time.Minute
	}
	needed := 2*drain + shutdownMargin
	if needed > timeout {
		timeout = needed
	}
	return timeout
}

func main() {
	zapLogger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer zapLogger.Sync()
	logger := logging.NewRecorder(zapLogger, 300)

	stateDir := os.Getenv("HNIEOJ_STATE_DIR")
	if stateDir == "" {
		stateDir = defaultStateDir
	}
	webAddr := os.Getenv("HNIEOJ_WEB_ADDR")
	if webAddr == "" {
		webAddr = defaultWebAddr
	}
	store := webui.NewStore(stateDir)
	if err := store.Ensure(); err != nil {
		zapLogger.Fatal("state dir init failed", zap.Error(err))
	}

	manager := node.NewManager(logger)
	if cfg, ok, err := store.LoadConfig(); err != nil {
		logger.Warn("stored config load failed", logging.Error(err))
	} else if ok {
		manager.SetConfig(*cfg)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	server := &http.Server{
		Addr:              webAddr,
		Handler:           webui.NewServer(store, manager, logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 收到关闭信号后先停止判题运行时（停止领取并在有界窗口内排空在途任务），
	// 再关闭 WebUI。main 会等待该 goroutine 完成，避免 ListenAndServe 一返回就退出。
	var shutdownWG sync.WaitGroup
	shutdownWG.Add(1)
	go func() {
		defer shutdownWG.Done()
		<-ctx.Done()
		cfg, _ := manager.Config()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeoutFor(cfg))
		defer cancel()
		if err := manager.Stop(shutdownCtx); err != nil {
			logger.Warn("judge runtime stop failed", logging.Error(err))
		}
		_ = server.Shutdown(shutdownCtx)
	}()

	if _, ok := manager.Config(); ok {
		go func() {
			startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := manager.Start(startCtx); err != nil {
				logger.Warn("auto start failed", logging.Error(err))
			}
		}()
	}

	logger.Info("webui started", logging.String("addr", webAddr), logging.String("stateDir", stateDir))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		zapLogger.Fatal("webui stopped", zap.Error(err))
	}
	// 必须等 shutdown 完成（运行时排空 + WebUI 关闭）后再返回。
	shutdownWG.Wait()
}
