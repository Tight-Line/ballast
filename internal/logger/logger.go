package logger

import (
	"io"
	"os"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New returns a logr.Logger for the named component, writing to stderr.
// level is one of "debug", "info", "warn", "error" (defaults to "info").
// format is "json" or "text" (defaults to "json").
func New(component, level, format string) logr.Logger {
	return newWithWriter(component, level, format, os.Stderr)
}

func newWithWriter(component, level, format string, w io.Writer) logr.Logger {
	var encoder zapcore.Encoder
	if format == "text" {
		encoder = zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	} else {
		cfg := zap.NewProductionEncoderConfig()
		cfg.EncodeTime = zapcore.ISO8601TimeEncoder
		encoder = zapcore.NewJSONEncoder(cfg)
	}
	core := zapcore.NewCore(encoder, zapcore.AddSync(w), parseLevel(level))
	z := zap.New(core)
	return zapr.NewLogger(z).WithName(component)
}

func parseLevel(level string) zapcore.Level {
	switch level {
	case "debug":
		return zapcore.DebugLevel
	case "warn":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}
