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

// LabelAbsent is a sentinel value stored in SelectorLabels for identity-label keys
// that were absent from the pod. Plugins translate this to a Kubernetes "!key"
// (does-not-exist) requirement so the selector matches only pods that truly lack
// that label — preventing a pod with app.kubernetes.io/component=server from
// incorrectly matching a profile whose pod had no component label.
const LabelAbsent = "--missing--"

// LabelPresent is a sentinel value stored in a selector for keys that must exist
// on the pod with any value. The metricscollector uses it to require the Ballast
// enrollment (mode) label, so only opted-in pods are measured even when unenrolled
// pods share the identity-tuple labels (common with the default tuple).
const LabelPresent = "--present--"

// MatchesSelector reports whether podLabels satisfies every requirement in
// selectorLabels. A value equal to LabelAbsent requires the key to be absent from
// podLabels; a value equal to LabelPresent requires the key to be present with any
// value; any other value requires an exact match. An empty selector matches every
// pod. This client-side filter is used because the metrics.k8s.io API ignores label
// selectors server-side.
func MatchesSelector(podLabels, selectorLabels map[string]string) bool {
	for k, v := range selectorLabels {
		switch v {
		case LabelAbsent:
			if _, present := podLabels[k]; present {
				return false
			}
		case LabelPresent:
			if _, present := podLabels[k]; !present {
				return false
			}
		default:
			if podLabels[k] != v {
				return false
			}
		}
	}
	return true
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
