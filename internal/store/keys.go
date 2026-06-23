package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// TupleHash returns a 16-character lowercase hex string derived from the
// SHA-256 of the sorted key=value pairs in labels. The result is
// deterministic and independent of map iteration order.
func TupleHash(labels map[string]string) string {
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

	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%x", sum[:8])
}

// MetricKey returns the Redis sorted-set key for a container/resource timeseries.
func MetricKey(tupleHash, container, resource string) string {
	return "ballast:metrics:" + tupleHash + ":" + container + ":" + resource
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
