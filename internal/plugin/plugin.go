package plugin

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// MetricsPlugin fetches per-container resource usage measurements for a workload identity.
// Implementations are compiled into the binary and registered by type name.
type MetricsPlugin interface {
	// Type returns the plugin type name, matching MetricsSource.spec.type.
	Type() string

	// FetchStats returns point-in-time resource usage for all containers of pods
	// matching id. Each ContainerStats entry is a single raw measurement; callers
	// accumulate these in Redis and compute statistical aggregations separately.
	FetchStats(ctx context.Context, id WorkloadIdentity, window TimeWindow) ([]ContainerStats, error)
}

// WorkloadIdentity holds the label tuple that identifies a WorkloadProfile.
type WorkloadIdentity struct {
	Labels map[string]string
}

// TimeWindow is the observation period passed to FetchStats. In-cluster plugins
// (kubernetesMetrics) return current measurements and ignore this field; external
// backends (e.g. Prometheus) use it to query historical data.
type TimeWindow struct {
	Start, End time.Time
}

// ContainerStats is a single point-in-time resource usage measurement for one
// container in one pod. Resource is "cpu", "memory", or "ephemeral-storage".
type ContainerStats struct {
	ContainerName string
	Resource      string // "cpu", "memory", or "ephemeral-storage"
	Value         resource.Quantity
	Timestamp     time.Time
}
