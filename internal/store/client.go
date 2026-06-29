package store

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client is the subset of the go-redis Cmdable interface used by this package.
// Tests substitute a *redis.Client backed by miniredis.
type Client interface {
	RPush(ctx context.Context, key string, values ...any) *redis.IntCmd
	LTrim(ctx context.Context, key string, start, stop int64) *redis.StatusCmd
	LRange(ctx context.Context, key string, start, stop int64) *redis.StringSliceCmd
	LLen(ctx context.Context, key string) *redis.IntCmd
	SetNX(ctx context.Context, key string, value any, expiration time.Duration) *redis.BoolCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
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
