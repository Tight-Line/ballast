package logger

import (
	"context"
	"testing"
)

func TestSetupLoggerProvider_DisabledWhenNoEndpoint(t *testing.T) {
	provider, shutdown, err := SetupLoggerProvider(context.Background(), ProviderConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider != nil {
		t.Errorf("expected nil provider when endpoint is empty, got %v", provider)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil no-op shutdown")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown returned error: %v", err)
	}
}

func TestSetupLoggerProvider_Protocols(t *testing.T) {
	for _, proto := range []string{"grpc", "http/protobuf", ""} {
		t.Run(proto, func(t *testing.T) {
			provider, shutdown, err := SetupLoggerProvider(context.Background(), ProviderConfig{
				OTLPEndpoint: "localhost:4317",
				OTLPProtocol: proto,
				OTLPInsecure: true,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if provider == nil {
				t.Fatal("expected non-nil provider when endpoint is set")
			}
			// Exporters connect lazily; shutdown flushes without a live collector.
			if err := shutdown(context.Background()); err != nil {
				t.Errorf("shutdown returned error: %v", err)
			}
		})
	}
}
