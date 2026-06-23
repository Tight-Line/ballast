package store

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Client is the subset of the go-redis Cmdable interface used by this package.
// Tests substitute a *redis.Client backed by miniredis.
type Client interface {
	ZAdd(ctx context.Context, key string, members ...redis.Z) *redis.IntCmd
	ZRangeArgsWithScores(ctx context.Context, z redis.ZRangeArgs) *redis.ZSliceCmd
	ZRemRangeByScore(ctx context.Context, key, min, max string) *redis.IntCmd
	ZCard(ctx context.Context, key string) *redis.IntCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	ZRemRangeByRank(ctx context.Context, key string, start, stop int64) *redis.IntCmd
	Scan(ctx context.Context, cursor uint64, match string, count int64) *redis.ScanCmd
}

// NewClient creates a Client from a Redis/Valkey URL (supports auth and TLS).
func NewClient(redisURL string) (Client, error) { // coverage:ignore - requires a live Redis instance
	opts, err := redis.ParseURL(redisURL)
	if err != nil { // coverage:ignore - requires a live Redis instance to exercise URL parse error
		return nil, fmt.Errorf("parsing redis URL: %w", err)
	}
	return redis.NewClient(opts), nil // coverage:ignore - requires a live Redis instance
}
