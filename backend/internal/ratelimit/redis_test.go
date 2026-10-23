package ratelimit

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func redisClients(t *testing.T) (*redis.Client, *redis.Client, string) {
	t.Helper()
	raw := os.Getenv("AEGIS_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("AEGIS_TEST_REDIS_URL is not set")
	}
	opts, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	a, b := redis.NewClient(opts), redis.NewClient(opts)
	t.Cleanup(func() { a.Close(); b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	return a, b, fmt.Sprintf("aegis:test:limiter:%d", time.Now().UnixNano())
}

func TestRedisSharedQuotaAtomicTTLAndHashedKey(t *testing.T) {
	a, b, namespace := redisClients(t)
	const window = 3 * time.Second
	first, second := NewWithWindow(a, namespace, window), NewWithWindow(b, namespace, window)
	ctx := context.Background()
	ip := "198.51.100.77"
	key := first.redisKey(ip)
	t.Cleanup(func() { a.Del(context.Background(), key) })
	if strings.Contains(key, ip) || !strings.HasPrefix(key, namespace+":login:") {
		t.Fatal("IP key is not hashed")
	}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l := first
			if i%2 != 0 {
				l = second
			}
			ok, retry, err := l.Allow(ctx, ip)
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				admitted.Add(1)
			} else if retry <= 0 || retry > window {
				t.Errorf("retry %v", retry)
			}
		}(i)
	}
	wg.Wait()
	if got := admitted.Load(); got != LoginLimit {
		t.Fatalf("admitted %d, want %d", got, LoginLimit)
	}
	before, err := a.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		ok, _, err := second.Allow(ctx, ip)
		if err != nil || ok {
			t.Fatalf("saturated admission %v %v", ok, err)
		}
	}
	count, err := a.Get(ctx, key).Int()
	if err != nil || count != LoginLimit {
		t.Fatalf("counter grew after rejection: %d %v", count, err)
	}
	after := a.PTTL(ctx, key).Val()
	if after > before {
		t.Fatalf("TTL extended: %v -> %v", before, after)
	}
	// Wait for Redis' own expiry, then verify a new fixed window is admitted.
	timer := time.NewTimer(after + 30*time.Millisecond)
	defer timer.Stop()
	<-timer.C
	ok, _, err := first.Allow(ctx, ip)
	if err != nil || !ok {
		t.Fatalf("expiry failed: %v %v", ok, err)
	}
}

func TestRedisErrorsFailClosed(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	client.Close()
	for _, l := range []*RedisLimiter{nil, New(nil, "namespace"), New(client, ""), New(client, "namespace"), NewWithWindow(client, "namespace", 0)} {
		ok, _, err := l.Allow(context.Background(), "peer")
		if ok || err == nil {
			t.Fatalf("failed open: %v %v", ok, err)
		}
	}
}

func TestRedisCorruptStateFailsClosed(t *testing.T) {
	a, _, namespace := redisClients(t)
	l := New(a, namespace)
	ctx := context.Background()
	key := l.redisKey("peer")
	t.Cleanup(func() { a.Del(context.Background(), key) })
	for _, value := range []string{"invalid", "0", "9223372036854775807"} {
		if err := a.Set(ctx, key, value, time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
		ok, _, err := l.Allow(ctx, "peer")
		if ok {
			t.Fatalf("corrupt/saturated state admitted: %s", value)
		}
		if value != "9223372036854775807" && err == nil {
			t.Fatalf("corrupt state did not error: %s", value)
		}
		if got := a.Get(ctx, key).Val(); got != value {
			t.Fatalf("counter mutated: %s", got)
		}
	}
	if err := a.Set(ctx, key, "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := l.Allow(ctx, "peer"); ok || err == nil {
		t.Fatal("missing TTL failed open")
	}
}
