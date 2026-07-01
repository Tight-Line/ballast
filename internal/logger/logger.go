package logger

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.opentelemetry.io/contrib/bridges/otelzap"
	otellog "go.opentelemetry.io/otel/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Options configures the logger built by New.
type Options struct {
	// Component names the logger (used as the logr/zap logger name and the OTel
	// instrumentation scope name).
	Component string

	// Level is one of "debug", "info", "warn", "error" (defaults to "info"). It
	// gates both the stdout and OTLP paths.
	Level string

	// Format is "json" or "text" (defaults to "json"). Applies to the stdout path.
	Format string

	// Stdout enables the human-facing encoder core writing to stderr. Disable it
	// to ship logs only via OTLP.
	Stdout bool

	// StdoutFields are static key/value pairs added to every stdout log line
	// only; they are never sent on the OTLP path. Use them to tag lines for a
	// downstream collector, e.g. {"otlp": true} so a stdout collector can skip
	// records already exported over OTLP and avoid double-ingesting them.
	StdoutFields map[string]any

	// LoggerProvider, when non-nil, adds an OTLP log core. Each structured
	// key/value logged is promoted to a top-level OTel log-record attribute.
	LoggerProvider otellog.LoggerProvider
}

// New returns a logr.Logger for the given options. The stdout path writes to
// stderr; the OTLP path (when a LoggerProvider is supplied) exports to the
// configured collector.
func New(opts Options) logr.Logger {
	return newWithWriter(opts, os.Stderr)
}

func newWithWriter(opts Options, w io.Writer) logr.Logger {
	var cores []zapcore.Core

	if opts.Stdout {
		var encoder zapcore.Encoder
		if opts.Format == "text" {
			encoder = zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
		} else {
			cfg := zap.NewProductionEncoderConfig()
			cfg.EncodeTime = zapcore.ISO8601TimeEncoder
			encoder = zapcore.NewJSONEncoder(cfg)
		}
		core := zapcore.NewCore(encoder, zapcore.AddSync(w), parseLevel(opts.Level))
		if fields := zapFields(opts.StdoutFields); len(fields) > 0 {
			core = core.With(fields)
		}
		cores = append(cores, core)
	}

	if opts.LoggerProvider != nil {
		// The bridge core enables every level and delegates filtering to the SDK,
		// so gate it to the same floor as stdout. NewIncreaseLevelCore only errors
		// when it would lower a core's level; since the bridge enables all levels,
		// raising to parseLevel(opts.Level) never errors.
		otelCore := otelzap.NewCore(opts.Component, otelzap.WithLoggerProvider(opts.LoggerProvider))
		leveled, err := zapcore.NewIncreaseLevelCore(otelCore, parseLevel(opts.Level))
		if err != nil { // coverage:ignore - bridge core enables all levels, so raising the floor never errors
			leveled = otelCore
		}
		cores = append(cores, leveled)
	}

	z := zap.New(zapcore.NewTee(cores...))
	return zapr.NewLogger(z).WithName(opts.Component)
}

// ParseFields decodes a JSON object of static log fields (as passed via
// --log-stdout-fields). An empty string yields a nil map.
func ParseFields(s string) (map[string]any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("parsing log fields JSON %q: %w", s, err)
	}
	return m, nil
}

// zapFields converts a static field map into zap fields with keys sorted so
// output ordering is deterministic.
func zapFields(m map[string]any) []zapcore.Field {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fields := make([]zapcore.Field, 0, len(m))
	for _, k := range keys {
		fields = append(fields, zap.Any(k, m[k]))
	}
	return fields
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
