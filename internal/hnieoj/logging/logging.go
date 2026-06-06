package logging

type Field struct {
	Key   string
	Value any
}

type Logger interface {
	Info(message string, fields ...Field)
	Warn(message string, fields ...Field)
}

type NopLogger struct{}

func (NopLogger) Info(string, ...Field) {}
func (NopLogger) Warn(string, ...Field) {}

func String(key, value string) Field {
	return Field{Key: key, Value: value}
}

func Int(key string, value int) Field {
	return Field{Key: key, Value: value}
}

func Int64(key string, value int64) Field {
	return Field{Key: key, Value: value}
}

func Error(err error) Field {
	return Field{Key: "error", Value: err}
}
