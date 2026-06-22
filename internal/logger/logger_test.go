package logger

import (
	"bytes"
	"strings"
	"testing"
)

func TestNew_SmokeTest(t *testing.T) {
	l := New("test", "info", "json")
	l.Info("smoke test")
}

func TestNewWithWriter_Levels(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", "invalid"} {
		t.Run(lvl, func(t *testing.T) {
			var buf bytes.Buffer
			l := newWithWriter("test", lvl, "json", &buf)
			l.Info("msg")
		})
	}
}

func TestNewWithWriter_JSONFormat(t *testing.T) {
	var buf bytes.Buffer
	l := newWithWriter("test", "info", "json", &buf)
	l.Info("hello")
	if !strings.HasPrefix(strings.TrimSpace(buf.String()), "{") {
		t.Errorf("expected JSON output, got: %q", buf.String())
	}
}

func TestNewWithWriter_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	l := newWithWriter("test", "info", "text", &buf)
	l.Info("hello")
	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Error("expected non-empty text output")
	}
	if strings.HasPrefix(out, "{") {
		t.Errorf("expected text output, not JSON, got: %q", out)
	}
}
