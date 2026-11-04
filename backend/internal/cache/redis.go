// Package cache contains Redis-backed ephemeral state used by the API.
// MySQL remains the source of truth for users and conversation messages.
package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store is the minimal cache interface used by the HTTP layer. Cache failures
// are deliberately non-fatal: callers can fall back to MySQL.
type Store interface {
	Get(context.Context, string) ([]byte, error)
	Set(context.Context, string, []byte, time.Duration) error
	Delete(context.Context, ...string) error
}

// ConcurrencyLimiter is a distributed, best-effort semaphore. It is used in
// addition to the local per-process limiter so multiple API instances share a
// global inference concurrency limit.
type ConcurrencyLimiter interface {
	Acquire(context.Context, string, int, time.Duration) (bool, error)
	Release(context.Context, string) error
}

type Redis struct {
	client *redis.Client
}

var acquireScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
redis.call('PEXPIRE', KEYS[1], ARGV[1])
if count > tonumber(ARGV[2]) then
  redis.call('DECR', KEYS[1])
  return 0
end
return 1
`)

var releaseScript = redis.NewScript(`
local count = redis.call('DECR', KEYS[1])
if count <= 0 then
  redis.call('DEL', KEYS[1])
end
return count
`)

func New(url string, poolSize, minIdle int) (*Redis, error) {
	options, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	if poolSize > 0 {
		options.PoolSize = poolSize
	}
	if minIdle > 0 {
		options.MinIdleConns = minIdle
	}
	options.MaxIdleConns = options.PoolSize
	options.ConnMaxIdleTime = 5 * time.Minute
	options.ConnMaxLifetime = 30 * time.Minute

	client := redis.NewClient(options)
	redisClient := &Redis{client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return redisClient, nil
}

func (r *Redis) Get(ctx context.Context, key string) ([]byte, error) {
	value, err := r.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	return value, err
}

func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

func (r *Redis) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return r.client.Del(ctx, keys...).Err()
}

func (r *Redis) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

func (r *Redis) Close() error {
	return r.client.Close()
}

func (r *Redis) Acquire(ctx context.Context, key string, limit int, ttl time.Duration) (bool, error) {
	if limit <= 0 {
		return true, nil
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	value, err := acquireScript.Run(ctx, r.client, []string{key}, ttl.Milliseconds(), limit).Int()
	return value == 1, err
}

func (r *Redis) Release(ctx context.Context, key string) error {
	return releaseScript.Run(ctx, r.client, []string{key}).Err()
}
