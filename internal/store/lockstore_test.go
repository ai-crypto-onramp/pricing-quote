package store

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedisLockStore(t *testing.T, ttl time.Duration) (*RedisLockStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { c.Close() })
	return NewRedisLockStoreFromClient(c, ttl), mr
}

func TestSetNXRoundTrip(t *testing.T) {
	ctx := context.Background()
	ls, _ := newTestRedisLockStore(t, 30*time.Second)

	ok, err := ls.SetNX(ctx, "lock:quote:q1", `{"rate":"1"}`, 0)
	if err != nil {
		t.Fatalf("setnx: %v", err)
	}
	if !ok {
		t.Fatalf("expected setnx success on fresh key")
	}

	ok2, err := ls.SetNX(ctx, "lock:quote:q1", `{"rate":"2"}`, 0)
	if err != nil {
		t.Fatalf("setnx second: %v", err)
	}
	if ok2 {
		t.Fatalf("expected setnx to fail on existing key")
	}

	v, err := ls.Get(ctx, "lock:quote:q1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v != `{"rate":"1"}` {
		t.Fatalf("got %q", v)
	}

	if err := ls.Del(ctx, "lock:quote:q1"); err != nil {
		t.Fatalf("del: %v", err)
	}
	if v, _ := ls.Get(ctx, "lock:quote:q1"); v != "" {
		t.Fatalf("expected empty after del, got %q", v)
	}
}

func TestSetNXGetMissing(t *testing.T) {
	ctx := context.Background()
	ls, _ := newTestRedisLockStore(t, 30*time.Second)
	if v, err := ls.Get(ctx, "lock:quote:missing"); err != nil || v != "" {
		t.Fatalf("expected empty for missing key, got %q err=%v", v, err)
	}
}

func TestLockedKeyAutoExpires(t *testing.T) {
	ctx := context.Background()
	ls, mr := newTestRedisLockStore(t, 30*time.Second)

	// Use a tiny TTL to verify auto-expiry via miniredis FastForward.
	ttl := 100 * time.Millisecond
	if _, err := ls.SetNX(ctx, "lock:quote:expire", "v", ttl); err != nil {
		t.Fatalf("setnx: %v", err)
	}
	if v, _ := ls.Get(ctx, "lock:quote:expire"); v != "v" {
		t.Fatalf("expected present before expiry, got %q", v)
	}

	mr.FastForward(ttl + time.Millisecond)

	if v, _ := ls.Get(ctx, "lock:quote:expire"); v != "" {
		t.Fatalf("expected empty after TTL, got %q", v)
	}
}

func TestLockedKeyDefaultTTLApplied(t *testing.T) {
	ctx := context.Background()
	ls, mr := newTestRedisLockStore(t, 42*time.Second)

	if _, err := ls.SetNX(ctx, "lock:quote:default", "v", 0); err != nil {
		t.Fatalf("setnx: %v", err)
	}
	ttl := mr.TTL("lock:quote:default")
	if ttl != 42*time.Second {
		t.Fatalf("expected default TTL 42s, got %v", ttl)
	}
}

func TestClaimAtomic(t *testing.T) {
	ctx := context.Background()
	ls, _ := newTestRedisLockStore(t, 30*time.Second)

	if _, err := ls.SetNX(ctx, "lock:quote:claim", `{"rate":"1.0"}`, 0); err != nil {
		t.Fatalf("setnx: %v", err)
	}

	val, ok, err := ls.Claim(ctx, "lock:quote:claim")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok {
		t.Fatalf("expected claim to succeed")
	}
	if val != `{"rate":"1.0"}` {
		t.Fatalf("got %q", val)
	}

	// Key must be deleted after claim.
	if v, _ := ls.Get(ctx, "lock:quote:claim"); v != "" {
		t.Fatalf("expected key deleted after claim, got %q", v)
	}

	// Second claim returns false with no error.
	_, ok2, err := ls.Claim(ctx, "lock:quote:claim")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if ok2 {
		t.Fatalf("expected second claim to fail")
	}
}

func TestClaimOnMissingKey(t *testing.T) {
	ctx := context.Background()
	ls, _ := newTestRedisLockStore(t, 30*time.Second)
	val, ok, err := ls.Claim(ctx, "lock:quote:never")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ok || val != "" {
		t.Fatalf("expected (false, \"\"), got (%v, %q)", ok, val)
	}
}

func TestRedisLockStorePing(t *testing.T) {
	ctx := context.Background()
	ls, _ := newTestRedisLockStore(t, 30*time.Second)
	if err := ls.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}