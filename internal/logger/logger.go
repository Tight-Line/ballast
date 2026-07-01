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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Options configures the logger built by New.
type Options struct {
	// Component names the logger (used as the logr/zap logger name and the OTel
	// instrumentation scope name).
	Component string

	// Level is one of "debug", "info", "warn", "error" (defaults to "info"). It
	// gates both the stdout and OTLP paths for any component without an override.
	Level string

	// LevelOverrides sets per-component log levels keyed by a substring of the
	// logger name (e.g. "webhook", "watcher", "collector", "adjuster"). A log
	// entry whose logger name contains a key is gated by that key's level;
	// entries matching no key use Level. Empty values are ignored. Name each
	// controller's logger with ControllerLogConstructor so these keys match.
	LevelOverrides map[string]string

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
	levels := newComponentLevels(opts.Level, opts.LevelOverrides)

	// Inner cores are enabled at every level; the componentLevelCore below does
	// the real gating so it can vary the floor per logger name.
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
		core := zapcore.NewCore(encoder, zapcore.AddSync(w), zapcore.DebugLevel)
		if fields := zapFields(opts.StdoutFields); len(fields) > 0 {
			core = core.With(fields)
		}
		cores = append(cores, core)
	}

	if opts.LoggerProvider != nil {
		cores = append(cores, otelzap.NewCore(opts.Component, otelzap.WithLoggerProvider(opts.LoggerProvider)))
	}

	gated := componentLevelCore{Core: zapcore.NewTee(cores...), levels: levels}
	z := zap.New(gated)
	return zapr.NewLogger(z).WithName(opts.Component)
}

// ControllerLogConstructor returns a controller-runtime LogConstructor that
// names the reconcile logger after component so per-component level overrides
// (see Options.LevelOverrides) match. It preserves the request's namespace/name
// fields that controller-runtime normally adds.
func ControllerLogConstructor(base logr.Logger, component string) func(*reconcile.Request) logr.Logger {
	named := base.WithName(component)
	return func(req *reconcile.Request) logr.Logger {
		if req == nil {
			return named
		}
		return named.WithValues("namespace", req.Namespace, "name", req.Name)
	}
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

// componentLevel pairs a logger-name substring with its log level.
type componentLevel struct {
	name  string
	level zapcore.Level
}

// componentLevels resolves the effective log level for a logger name: the first
// matching override wins, otherwise the default level applies.
type componentLevels struct {
	def       zapcore.Level
	min       zapcore.Level
	overrides []componentLevel
}

func newComponentLevels(def string, overrides map[string]string) componentLevels {
	d := parseLevel(def)
	cl := componentLevels{def: d, min: d}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := overrides[k]
		if v == "" {
			continue
		}
		lvl := parseLevel(v)
		cl.overrides = append(cl.overrides, componentLevel{name: k, level: lvl})
		if lvl < cl.min {
			cl.min = lvl
		}
	}
	return cl
}

func (c componentLevels) levelFor(name string) zapcore.Level {
	for _, o := range c.overrides {
		if strings.Contains(name, o.name) {
			return o.level
		}
	}
	return c.def
}

// componentLevelCore gates entries by a per-logger-name level before delegating
// to the wrapped core. It lets a single core apply different level floors to
// different components (keyed on the entry's logger name).
type componentLevelCore struct {
	zapcore.Core
	levels componentLevels
}

func (c componentLevelCore) Enabled(l zapcore.Level) bool {
	// Enabled has no logger name, so answer conservatively: enabled if any
	// component would log at this level. Check does the precise per-name gating.
	return l >= c.levels.min
}

func (c componentLevelCore) With(fields []zapcore.Field) zapcore.Core {
	return componentLevelCore{Core: c.Core.With(fields), levels: c.levels}
}

func (c componentLevelCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if ent.Level < c.levels.levelFor(ent.LoggerName) {
		return ce
	}
	return c.Core.Check(ent, ce)
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
