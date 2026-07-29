package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// policyHashPrefix separates the governing policy from the label pairs in a
// measurement hash. The NUL byte is illegal in a Kubernetes label key, so no
// real label can produce the same canonical line and alias a different policy.
const policyHashPrefix = "\x00policy="

// TupleHash returns a 16-character lowercase hex string derived from the
// SHA-256 of the sorted key=value pairs in labels. The result is
// deterministic and independent of map iteration order.
//
// This identifies a workload identity tuple, and is used for bookkeeping that is
// genuinely per-tuple (such as metrics-plugin backoff). Redis measurement keys
// use MeasurementHash instead, because the samples they hold belong to one
// profile rather than to the tuple as a whole.
func TupleHash(labels map[string]string) string {
	sum := sha256.Sum256([]byte(canonicalLabels(labels)))
	return fmt.Sprintf("%x", sum[:8])
}

// MeasurementHash returns the 16-character hex hash identifying the Redis key
// namespace a single WorkloadProfile owns. It covers both the identity tuple and
// the governing policy, so two profiles that share a tuple but resolve to
// different policies never write into one series.
//
// Sharing a series across policies is not merely wasteful: samples carry no
// per-sample timestamps, so two profiles appending to one key would inflate the
// count, distort the distribution, and halve the effective retention window. A
// per-profile namespace also lets the profile finalizer purge its own keys
// without reference counting, since no sibling can be reading them.
//
// policyKey is the canonical identity of the governing policy, as produced by
// the profile's status.policyRef; an empty policyKey means no policy matched.
func MeasurementHash(labels map[string]string, policyKey string) string {
	sum := sha256.Sum256([]byte(canonicalLabels(labels) + policyHashPrefix + policyKey + "\n"))
	return fmt.Sprintf("%x", sum[:8])
}

// canonicalLabels serializes labels as sorted "key=value\n" lines, giving a
// representation that is independent of map iteration order.
func canonicalLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte('\n')
	}
	return b.String()
}

// MetricKey returns the Redis sorted-set key for a container/resource timeseries.
// hash is the owning profile's MeasurementHash.
func MetricKey(hash, container, resource string) string {
	return "ballast:metrics:" + hash + ":" + container + ":" + resource
}

// AllKeysForHash scans Redis for all metric keys that belong to tupleHash.
func AllKeysForHash(ctx context.Context, c Client, tupleHash string) ([]string, error) {
	pattern := "ballast:metrics:" + tupleHash + ":*"
	var allKeys []string
	var cursor uint64
	for {
		keys, next, err := c.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil { // coverage:ignore - Scan failure requires a broken Redis instance
			return nil, fmt.Errorf("scanning keys for hash %s: %w", tupleHash, err)
		}
		allKeys = append(allKeys, keys...)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return allKeys, nil
}
