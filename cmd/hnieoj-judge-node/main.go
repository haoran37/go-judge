package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

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
	"go.uber.org/zap"
)

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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	httpClient := &http.Client{Timeout: cfg.HnieOJ.RequestTimeout}
	cred, err := auth.Authenticate(ctx, *cfg, httpClient)
	if err != nil {
		logger.Fatal("auth failed", zap.Error(err))
	}
	logger.Info("auth succeeded", zap.String("nodeType", cfg.Node.Type), zap.String("nodeName", cfg.Node.Name))

	appLogger := zapAdapter{logger: logger}
	rep := buildReporter(*cfg, httpClient, cred, appLogger)
	testdataClient := testdata.New(cfg.HnieOJ.BaseURL, cfg.Testdata.CacheRoot, httpClient, cred, appLogger)
	runnerClient := runner.New(cfg.GoJudge.Endpoint, cfg.GoJudge.AuthToken, httpClient, appLogger)
	proc := processor.New(testdataClient, runnerClient, rep, cred, appLogger)

	var running atomic.Int64
	heartbeat.New(*cfg, cred, httpClient, appLogger, &running).Start(ctx)
	logger.Info("node started", zap.String("nodeName", cfg.Node.Name), zap.String("nodeType", cfg.Node.Type), zap.Int("maxConcurrency", cfg.Node.MaxConcurrency))

	handler := limitedHandler(cfg.Node.MaxConcurrency, &running, proc.Process)
	if fixturePath != "" {
		if err := runFixture(ctx, fixturePath, handler); err != nil {
			logger.Fatal("fixture failed", zap.Error(err))
		}
		return
	}

	consumer := mq.New(cfg.RabbitMQ, appLogger)
	if err := consumer.Consume(ctx, handler); err != nil && !errors.Is(err, context.Canceled) {
		logger.Fatal("rabbitmq consumer stopped", zap.Error(err))
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
