package store

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// AddSample appends valueStr to the list at key, trims to the most recent cap
// entries (keeping newest), and records the first-seen timestamp via SET NX.
// cap <= 0 skips the trim.
func AddSample(ctx context.Context, c Client, key string, timestampMs int64, valueStr string, maxEntries int64) error {
	if err := c.RPush(ctx, key, valueStr).Err(); err != nil { // coverage:ignore - Redis error
		return fmt.Errorf("RPUSH %s: %w", key, err)
	}
	if maxEntries > 0 {
		if err := c.LTrim(ctx, key, -maxEntries, -1).Err(); err != nil { // coverage:ignore - Redis error
			return fmt.Errorf("LTRIM %s: %w", key, err)
		}
	}
	if err := c.SetNX(ctx, firstSeenKey(key), strconv.FormatInt(timestampMs, 10), 0).Err(); err != nil { // coverage:ignore - Redis error
		return fmt.Errorf("SETNX %s: %w", firstSeenKey(key), err)
	}
	return nil
}

// QueryAll returns all values in the list at key, oldest first.
func QueryAll(ctx context.Context, c Client, key string) ([]string, error) {
	vals, err := c.LRange(ctx, key, 0, -1).Result()
	if err != nil { // coverage:ignore - Redis error requires a broken instance
		return nil, fmt.Errorf("LRANGE %s: %w", key, err)
	}
	return vals, nil
}

// FirstSeenMs returns the Unix timestamp (ms) when the first sample was stored
// at key. Returns 0, nil if no sample has been recorded yet.
func FirstSeenMs(ctx context.Context, c Client, key string) (int64, error) {
	v, err := c.Get(ctx, firstSeenKey(key)).Result()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil { // coverage:ignore - Redis error requires a broken instance
		return 0, fmt.Errorf("GET %s: %w", firstSeenKey(key), err)
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil { // coverage:ignore - only on corrupted Redis data
		return 0, fmt.Errorf("parsing first_seen for %s: %w", key, err)
	}
	return ms, nil
}

// SampleCount returns the number of entries in the list at key.
func SampleCount(ctx context.Context, c Client, key string) (int64, error) {
	n, err := c.LLen(ctx, key).Result()
	if err != nil { // coverage:ignore - Redis error requires a broken instance
		return 0, fmt.Errorf("LLEN %s: %w", key, err)
	}
	return n, nil
}

// DeleteKey removes the list at key. The associated first_seen key
// (key + ":first_seen") matches AllKeysForHash's glob pattern and is
// deleted in the same cleanup pass.
func DeleteKey(ctx context.Context, c Client, key string) error {
	return c.Del(ctx, key).Err()
}

func firstSeenKey(key string) string {
	return key + ":first_seen"
}
