/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package metrics

import (
	"errors"
	"testing"

	promclient "github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestFilteredGatherer_DropsPrefixes pins the anti-duplication filter on the
// Prometheus→OTLP bridge: families mirrored into the registry by the OTel
// Prometheus exporter (ballast_*, otel_scope_*, target_info) must not be
// bridged back out, while controller-runtime families pass through.
func TestFilteredGatherer_DropsPrefixes(t *testing.T) {
	reg := promclient.NewRegistry()
	for _, name := range []string{
		"ballast_pods_processed_total",
		"otel_scope_info",
		"target_info",
		"workqueue_depth",
		"controller_runtime_reconcile_total",
	} {
		c := promclient.NewCounter(promclient.CounterOpts{Name: name, Help: "test"})
		if err := reg.Register(c); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	g := filteredGatherer{
		inner:        reg,
		dropPrefixes: []string{"ballast_", "otel_scope_", "target_info"},
	}
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	got := make(map[string]bool, len(families))
	for _, mf := range families {
		got[mf.GetName()] = true
	}
	for _, want := range []string{"workqueue_depth", "controller_runtime_reconcile_total"} {
		if !got[want] {
			t.Errorf("family %s was dropped, want kept", want)
		}
	}
	for _, dropped := range []string{"ballast_pods_processed_total", "otel_scope_info", "target_info"} {
		if got[dropped] {
			t.Errorf("family %s was kept, want dropped", dropped)
		}
	}
}

func TestFilteredGatherer_PropagatesError(t *testing.T) {
	wantErr := errors.New("gather failed")
	g := filteredGatherer{
		inner: promclient.GathererFunc(func() ([]*dto.MetricFamily, error) {
			return nil, wantErr
		}),
	}
	if _, err := g.Gather(); !errors.Is(err, wantErr) {
		t.Errorf("Gather error = %v, want %v", err, wantErr)
	}
}
