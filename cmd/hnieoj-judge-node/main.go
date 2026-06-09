package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/node"
	"github.com/criyle/go-judge/internal/hnieoj/webui"
	"go.uber.org/zap"
)

const (
	webAddr         = ":3723"
	defaultStateDir = "/var/lib/hnieoj-judge-node"
)

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

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = manager.Stop(shutdownCtx)
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
}
