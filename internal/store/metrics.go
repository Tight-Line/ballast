package store

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// ScoredValue pairs a sorted-set member with its score (a Unix timestamp in ms).
type ScoredValue struct {
	TimestampMs int64
	Value       string
}

// AddSample appends a timestamped sample to the sorted set at key via ZADD.
// timestampMs is stored as the score; valueStr is the member.
func AddSample(ctx context.Context, c Client, key string, timestampMs int64, valueStr string) error {
	return c.ZAdd(ctx, key, redis.Z{Score: float64(timestampMs), Member: valueStr}).Err()
}

// QueryWindow returns all samples whose score falls in [startMs, endMs].
func QueryWindow(ctx context.Context, c Client, key string, startMs, endMs int64) ([]ScoredValue, error) {
	zs, err := c.ZRangeArgsWithScores(ctx, redis.ZRangeArgs{
		Key:     key,
		Start:   strconv.FormatInt(startMs, 10),
		Stop:    strconv.FormatInt(endMs, 10),
		ByScore: true,
	}).Result()
	if err != nil { // coverage:ignore - Redis error requires a broken instance
		return nil, fmt.Errorf("querying window for %s: %w", key, err)
	}
	out := make([]ScoredValue, len(zs))
	for i, z := range zs {
		out[i] = ScoredValue{
			TimestampMs: int64(z.Score),
			Value:       fmt.Sprint(z.Member),
		}
	}
	return out, nil
}

// ExpireOlderThan removes all entries with scores strictly less than cutoffMs.
func ExpireOlderThan(ctx context.Context, c Client, key string, cutoffMs int64) error {
	return c.ZRemRangeByScore(ctx, key, "-inf", "("+strconv.FormatInt(cutoffMs, 10)).Err()
}

// SampleCount returns the number of entries in the sorted set via ZCARD.
func SampleCount(ctx context.Context, c Client, key string) (int64, error) {
	n, err := c.ZCard(ctx, key).Result()
	if err != nil { // coverage:ignore - Redis error requires a broken instance
		return 0, fmt.Errorf("ZCARD %s: %w", key, err)
	}
	return n, nil
}

// TimeRange returns the timestamps of the oldest and newest entries.
// Returns 0, 0, nil for an empty or non-existent key.
func TimeRange(ctx context.Context, c Client, key string) (firstMs, lastMs int64, err error) {
	first, err := c.ZRangeArgsWithScores(ctx, redis.ZRangeArgs{Key: key, Start: 0, Stop: 0}).Result()
	if err != nil { // coverage:ignore - Redis error requires a broken instance
		return 0, 0, fmt.Errorf("ZRANGE first %s: %w", key, err)
	}
	last, err := c.ZRangeArgsWithScores(ctx, redis.ZRangeArgs{Key: key, Start: -1, Stop: -1}).Result()
	if err != nil { // coverage:ignore - Redis error requires a broken instance
		return 0, 0, fmt.Errorf("ZRANGE last %s: %w", key, err)
	}
	if len(first) == 0 || len(last) == 0 {
		return 0, 0, nil
	}
	return int64(first[0].Score), int64(last[0].Score), nil
}

// DeleteKey removes the sorted set at key via DEL.
func DeleteKey(ctx context.Context, c Client, key string) error {
	return c.Del(ctx, key).Err()
}

// EnforceReservoirCap trims the oldest entries so at most maxEntries remain.
// Uses ZREMRANGEBYRANK 0 -(maxEntries+1), which is a no-op when the set is
// within capacity.
func EnforceReservoirCap(ctx context.Context, c Client, key string, maxEntries int64) error {
	return c.ZRemRangeByRank(ctx, key, 0, -(maxEntries + 1)).Err()
}
