package pricing

import (
	"context"
	"time"

	redis "github.com/redis/go-redis/v9"
)

// redisLockBackend implements LockBackend against a Redis server using SET NX EX
// for SetNX and a Lua script for atomic Claim.
type redisLockBackend struct {
	client *redis.Client
	script *redis.Script
}

// newRedisLockBackend dials the Redis server at url and returns a LockBackend
// backed by it. Returns an error if the connection cannot be established.
func newRedisLockBackend(ctx context.Context, url string) (LockBackend, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	c := redis.NewClient(opts)
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return &redisLockBackend{
		client: c,
		script: redis.NewScript(claimLuaScript),
	}, nil
}

func (r *redisLockBackend) SetNX(key, value string, ttl time.Duration) bool {
	ok, err := r.client.SetNX(context.Background(), key, value, ttl).Result()
	if err != nil {
		return false
	}
	return ok
}

func (r *redisLockBackend) Get(key string) (string, bool) {
	v, err := r.client.Get(context.Background(), key).Result()
	if err != nil {
		return "", false
	}
	return v, true
}

func (r *redisLockBackend) Del(key string) bool {
	n, err := r.client.Del(context.Background(), key).Result()
	if err != nil {
		return false
	}
	return n > 0
}

// Claim runs the Lua script that returns the prior value and deletes the key
// in a single round-trip.
func (r *redisLockBackend) Claim(key string) (string, bool) {
	res, err := r.script.Run(context.Background(), r.client, []string{key}).Result()
	if err != nil {
		return "", false
	}
	if res == nil {
		return "", false
	}
	switch v := res.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	default:
		return "", false
	}
}

// Close releases the Redis connection.
func (r *redisLockBackend) Close() error { return r.client.Close() }

// Ready pings the Redis server and reports reachability.
func (r *redisLockBackend) Ready() bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return r.client.Ping(ctx).Err() == nil
}

// initLockBackend returns a LockBackend for the configured Redis URL. When
// dialing fails (Redis unavailable, missing REDIS_URL), it falls back to the
// in-memory LockStore so the service still functions for local development and
// tests. A warning is logged via the provided logger when falling back.
func initLockBackend(cfg Config, log *logger) LockBackend {
	if cfg.RedisURL == "" {
		if log != nil {
			log.Warn("REDIS_URL unset; using in-memory lock store")
		}
		return NewLockStore()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, err := newRedisLockBackend(ctx, cfg.RedisURL)
	if err != nil {
		if log != nil {
			log.Warn("redis unavailable, falling back to in-memory lock store: " + err.Error())
		}
		return NewLockStore()
	}
	if log != nil {
		log.Info("connected to Redis at " + maskURL(cfg.RedisURL))
	}
	return b
}

// closeLockBackend closes the backend if it implements io.Closer (e.g. the
// Redis-backed adapter). In-memory backends are a no-op.
func closeLockBackend(b LockBackend) {
	if cl, ok := b.(interface{ Close() error }); ok {
		_ = cl.Close()
	}
}