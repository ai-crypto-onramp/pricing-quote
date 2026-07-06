package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// claimScript atomically returns the prior value and deletes the key in a
// single round-trip. Returns nil if the key did not exist.
//
//	KEYS[1] = the lock key
var claimScript = redis.NewScript(`
local v = redis.call("GET", KEYS[1])
if v == false then
  return false
end
redis.call("DEL", KEYS[1])
return v
`)

// ErrLockHeld is returned by SetNX when the key already exists.
var ErrLockHeld = errors.New("lock already held")

// LockStore is the Redis locked-quote store with TTL and atomic claim.
type LockStore interface {
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	Get(ctx context.Context, key string) (string, error)
	Del(ctx context.Context, key string) error
	// Claim atomically returns the prior value and deletes the key in a single
	// round-trip. Returns ("", false, nil) when the key did not exist.
	Claim(ctx context.Context, key string) (string, bool, error)
	Ping(ctx context.Context) error
	Close() error
}

// RedisLockStore is a LockStore backed by go-redis.
type RedisLockStore struct {
	c      *redis.Client
	ttl    time.Duration
	script *redis.Script
}

// NewRedisLockStore connects to Redis at url using the default locked-quote TTL.
func NewRedisLockStore(ctx context.Context, url string, ttl time.Duration) (*RedisLockStore, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	c := redis.NewClient(opts)
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &RedisLockStore{c: c, ttl: ttl, script: claimScript}, nil
}

// NewRedisLockStoreFromClient wraps an existing redis.Client (used in tests
// with miniredis). ttl is the default TTL for SetNX when ttl <= 0.
func NewRedisLockStoreFromClient(c *redis.Client, ttl time.Duration) *RedisLockStore {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &RedisLockStore{c: c, ttl: ttl, script: claimScript}
}

// SetNX sets key=value with TTL iff key does not already exist. Returns
// (true, nil) on success and (false, nil) if the key was already present.
func (l *RedisLockStore) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = l.ttl
	}
	ok, err := l.c.SetNX(ctx, key, value, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("setnx: %w", err)
	}
	return ok, nil
}

// Get returns the value at key. Returns ("", false, nil) if missing.
func (l *RedisLockStore) Get(ctx context.Context, key string) (string, error) {
	v, err := l.c.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get: %w", err)
	}
	return v, nil
}

// Del removes the key. No-op if the key is absent.
func (l *RedisLockStore) Del(ctx context.Context, key string) error {
	if err := l.c.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("del: %w", err)
	}
	return nil
}

// Claim atomically returns the prior value and deletes the key in a single
// round-trip via a Lua script. Returns ("", false, nil) when the key was absent.
func (l *RedisLockStore) Claim(ctx context.Context, key string) (string, bool, error) {
	res, err := l.script.Run(ctx, l.c, []string{key}).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("claim script: %w", err)
	}
	s, ok := res.(string)
	if !ok {
		return "", false, fmt.Errorf("claim script: unexpected type %T", res)
	}
	return s, true, nil
}

func (l *RedisLockStore) Ping(ctx context.Context) error { return l.c.Ping(ctx).Err() }
func (l *RedisLockStore) Close() error                   { return l.c.Close() }

var _ LockStore = (*RedisLockStore)(nil)