package logger

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestNew_SmokeTest(t *testing.T) {
	l := New(Options{Component: "test", Level: "info", Format: "json", Stdout: true})
	l.Info("smoke test")
}

func TestNewWithWriter_Levels(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", "invalid"} {
		t.Run(lvl, func(t *testing.T) {
			var buf bytes.Buffer
			l := newWithWriter(Options{Component: "test", Level: lvl, Format: "json", Stdout: true}, &buf)
			l.Info("msg")
		})
	}
}

func TestNewWithWriter_JSONFormat(t *testing.T) {
	var buf bytes.Buffer
	l := newWithWriter(Options{Component: "test", Level: "info", Format: "json", Stdout: true}, &buf)
	l.Info("hello")
	if !strings.HasPrefix(strings.TrimSpace(buf.String()), "{") {
		t.Errorf("expected JSON output, got: %q", buf.String())
	}
}

func TestNewWithWriter_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	l := newWithWriter(Options{Component: "test", Level: "info", Format: "text", Stdout: true}, &buf)
	l.Info("hello")
	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Error("expected non-empty text output")
	}
	if strings.HasPrefix(out, "{") {
		t.Errorf("expected text output, not JSON, got: %q", out)
	}
}

func TestNewWithWriter_StdoutFields(t *testing.T) {
	var buf bytes.Buffer
	l := newWithWriter(Options{
		Component:    "test",
		Level:        "info",
		Format:       "json",
		Stdout:       true,
		StdoutFields: map[string]any{"otlp": true, "site": "client"},
	}, &buf)
	l.Info("hello")
	out := buf.String()
	if !strings.Contains(out, `"otlp":true`) {
		t.Errorf("expected static field otlp=true in output, got: %q", out)
	}
	if !strings.Contains(out, `"site":"client"`) {
		t.Errorf("expected static field site=client in output, got: %q", out)
	}
}

func TestNewWithWriter_StdoutDisabled(t *testing.T) {
	var buf bytes.Buffer
	l := newWithWriter(Options{Component: "test", Level: "info", Format: "json", Stdout: false}, &buf)
	l.Info("hello")
	if buf.Len() != 0 {
		t.Errorf("expected no stdout output when Stdout is false, got: %q", buf.String())
	}
}

// captureExporter is an in-memory sdklog.Exporter that records everything it
// receives. Paired with a SimpleProcessor it captures records synchronously.
type captureExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *captureExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = append(e.records, records...)
	return nil
}

func (e *captureExporter) Shutdown(context.Context) error   { return nil }
func (e *captureExporter) ForceFlush(context.Context) error { return nil }

func (e *captureExporter) snapshot() []sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdklog.Record(nil), e.records...)
}

func newCaptureProvider(exp *captureExporter) *sdklog.LoggerProvider {
	return sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
}

func TestNewWithWriter_OTLPPathPromotesAttributesAndKeepsFieldsOffOTLP(t *testing.T) {
	exp := &captureExporter{}
	provider := newCaptureProvider(exp)
	defer func() { _ = provider.Shutdown(context.Background()) }()

	var buf bytes.Buffer
	l := newWithWriter(Options{
		Component:      "test",
		Level:          "info",
		Format:         "json",
		Stdout:         true,
		StdoutFields:   map[string]any{"otlp": true},
		LoggerProvider: provider,
	}, &buf)

	l.Info("hello", "workload", "web")

	records := exp.snapshot()
	if len(records) != 1 {
		t.Fatalf("expected 1 exported record, got %d", len(records))
	}
	rec := records[0]
	if got := rec.Body().AsString(); got != "hello" {
		t.Errorf("expected body %q, got %q", "hello", got)
	}

	attrs := map[string]string{}
	rec.WalkAttributes(func(kv otellog.KeyValue) bool {
		attrs[kv.Key] = kv.Value.AsString()
		return true
	})
	if attrs["workload"] != "web" {
		t.Errorf("expected structured key workload=web promoted to a top-level attribute, got attrs: %v", attrs)
	}
	// The stdout-only marker must not leak onto the OTLP record.
	if _, ok := attrs["otlp"]; ok {
		t.Errorf("stdout-only field otlp leaked onto the OTLP record: %v", attrs)
	}
	// It must still appear on the stdout line.
	if !strings.Contains(buf.String(), `"otlp":true`) {
		t.Errorf("expected otlp marker on stdout line, got: %q", buf.String())
	}
}

func TestNewWithWriter_OTLPRespectsLevel(t *testing.T) {
	exp := &captureExporter{}
	provider := newCaptureProvider(exp)
	defer func() { _ = provider.Shutdown(context.Background()) }()

	l := newWithWriter(Options{
		Component:      "test",
		Level:          "error",
		Format:         "json",
		Stdout:         false,
		LoggerProvider: provider,
	}, &bytes.Buffer{})

	l.Info("below the floor")

	if records := exp.snapshot(); len(records) != 0 {
		t.Errorf("expected info records to be gated out at error level, got %d", len(records))
	}
}

func TestComponentLevelOverrides(t *testing.T) {
	var buf bytes.Buffer
	l := newWithWriter(Options{
		Component: "setup",
		Level:     "info",
		Format:    "json",
		Stdout:    true,
		LevelOverrides: map[string]string{
			"collector": "debug",
			"webhook":   "error",
			"watcher":   "", // empty override is ignored (inherits the global level)
		},
	}, &buf)

	// collector override = debug: a V(1) (debug) line on a collector-named logger emits.
	l.WithName("metricscollector").V(1).Info("collector-debug")
	// default level = info: a debug line on an unmatched logger is dropped.
	l.WithName("other").V(1).Info("other-debug")
	// webhook override = error: an info line is dropped, an error line emits.
	l.WithName("webhook").Info("webhook-info")
	l.WithName("webhook").Error(nil, "webhook-error")
	// watcher inherits info: an info line emits, a debug line is dropped.
	l.WithName("workloadwatcher-pod").Info("watcher-info")
	l.WithName("workloadwatcher-pod").V(1).Info("watcher-debug")

	out := buf.String()
	for _, want := range []string{"collector-debug", "webhook-error", "watcher-info"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output, got: %q", want, out)
		}
	}
	for _, notWant := range []string{"other-debug", "webhook-info", "watcher-debug"} {
		if strings.Contains(out, notWant) {
			t.Errorf("expected %q to be gated out, got: %q", notWant, out)
		}
	}
}

func TestComponentLevelCore_EnabledUsesMin(t *testing.T) {
	var buf bytes.Buffer
	withOverride := newWithWriter(Options{
		Component: "setup", Level: "info", Format: "json", Stdout: true,
		LevelOverrides: map[string]string{"collector": "debug"},
	}, &buf)
	if !withOverride.V(1).Enabled() {
		t.Error("expected debug enabled when a component lowers the floor to debug")
	}

	noOverride := newWithWriter(Options{Component: "setup", Level: "info", Format: "json", Stdout: true}, &buf)
	if noOverride.V(1).Enabled() {
		t.Error("expected debug disabled when no component lowers the floor")
	}
}

func TestControllerLogConstructor(t *testing.T) {
	var buf bytes.Buffer
	base := newWithWriter(Options{Component: "setup", Level: "info", Format: "json", Stdout: true}, &buf)
	ctor := ControllerLogConstructor(base, "collector")

	ctor(nil).Info("no-req")
	ctor(&reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "nm"}}).Info("with-req")

	out := buf.String()
	if !strings.Contains(out, `"logger":"setup.collector"`) {
		t.Errorf("expected logger named setup.collector, got: %q", out)
	}
	if !strings.Contains(out, `"namespace":"ns"`) || !strings.Contains(out, `"name":"nm"`) {
		t.Errorf("expected request namespace/name fields, got: %q", out)
	}
}

func TestParseFields(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		m, err := ParseFields("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if m != nil {
			t.Errorf("expected nil map for empty input, got %v", m)
		}
	})
	t.Run("whitespace", func(t *testing.T) {
		m, err := ParseFields("   ")
		if err != nil || m != nil {
			t.Fatalf("expected nil map, nil err for whitespace, got %v, %v", m, err)
		}
	})
	t.Run("valid", func(t *testing.T) {
		m, err := ParseFields(`{"otlp":true,"n":3}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if m["otlp"] != true {
			t.Errorf("expected otlp=true, got %v", m["otlp"])
		}
	})
	t.Run("invalid", func(t *testing.T) {
		if _, err := ParseFields(`{not json`); err == nil {
			t.Error("expected error for invalid JSON")
		}
	})
}
