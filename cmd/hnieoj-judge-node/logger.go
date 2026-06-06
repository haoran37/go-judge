package main

import (
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"go.uber.org/zap"
)

type zapAdapter struct {
	logger *zap.Logger
}

func (a zapAdapter) Info(message string, fields ...logging.Field) {
	a.logger.Info(message, toZapFields(fields)...)
}

func (a zapAdapter) Warn(message string, fields ...logging.Field) {
	a.logger.Warn(message, toZapFields(fields)...)
}

func toZapFields(fields []logging.Field) []zap.Field {
	out := make([]zap.Field, 0, len(fields))
	for _, f := range fields {
		out = append(out, zap.Any(f.Key, f.Value))
	}
	return out
}
