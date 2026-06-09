package logging

import (
	"sync"
	"time"

	"go.uber.org/zap"
)

type Entry struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

type Recorder struct {
	mu      sync.Mutex
	entries []Entry
	limit   int
	base    *zap.Logger
}

func NewRecorder(base *zap.Logger, limit int) *Recorder {
	if limit <= 0 {
		limit = 200
	}
	return &Recorder{base: base, limit: limit}
}

func (r *Recorder) Info(message string, fields ...Field) {
	r.write("info", message, fields...)
	if r.base != nil {
		r.base.Info(message, toZapFields(fields)...)
	}
}

func (r *Recorder) Warn(message string, fields ...Field) {
	r.write("warn", message, fields...)
	if r.base != nil {
		r.base.Warn(message, toZapFields(fields)...)
	}
}

func (r *Recorder) Recent() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, len(r.entries))
	copy(out, r.entries)
	return out
}

func (r *Recorder) write(level, message string, fields ...Field) {
	entry := Entry{
		Time:    time.Now(),
		Level:   level,
		Message: message,
		Fields:  map[string]any{},
	}
	for _, field := range fields {
		entry.Fields[field.Key] = field.Value
	}
	if len(entry.Fields) == 0 {
		entry.Fields = nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry)
	if len(r.entries) > r.limit {
		r.entries = r.entries[len(r.entries)-r.limit:]
	}
}

func toZapFields(fields []Field) []zap.Field {
	out := make([]zap.Field, 0, len(fields))
	for _, f := range fields {
		out = append(out, zap.Any(f.Key, f.Value))
	}
	return out
}
