// Package ratelimit provides a shared, fail-closed login quota.
package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limiter admits at most the configured quota and reports the remaining window
// on rejection. Callers must fail closed when err is non-nil.
type Limiter interface {
	Allow(ctx context.Context, key string) (allowed bool, retryAfter time.Duration, err error)
}

const LoginLimit = 10
const LoginWindow = time.Minute

// The first admitted request starts a fixed window. Rejected requests neither
// increment the saturated counter nor extend its TTL. All operations are atomic.
var fixedWindow = redis.NewScript(`
local count = redis.call('GET', KEYS[1])
if not count then
  redis.call('SET', KEYS[1], 1, 'PX', ARGV[1])
  return {1, 0}
end
local n = tonumber(count)
if not n or n < 1 then
  return redis.error_reply('invalid rate limit state')
end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  return redis.error_reply('rate limit state has no expiry')
end
if n >= tonumber(ARGV[2]) then
  return {0, math.max(ttl, 1)}
end
redis.call('INCR', KEYS[1])
return {1, 0}
`)

// RedisLimiter is safe for concurrent use and shares quota across clients and
// servers using the same deployment namespace. It never stores plaintext IPs.
type RedisLimiter struct {
	client    redis.UniversalClient
	namespace string
	window    time.Duration
}

var _ Limiter = (*RedisLimiter)(nil)

// New constructs the production login limiter: ten requests per minute. The
// deployment entrypoint supplies a fixed namespace shared by every instance.
func New(client redis.UniversalClient, namespace string) *RedisLimiter {
	return NewWithWindow(client, namespace, LoginWindow)
}

// NewWithWindow permits short windows in integration tests; the quota remains
// ten. Invalid configuration is rejected by Allow rather than disabling limits.
func NewWithWindow(client redis.UniversalClient, namespace string, window time.Duration) *RedisLimiter {
	return &RedisLimiter{client: client, namespace: namespace, window: window}
}

func (l *RedisLimiter) redisKey(key string) string {
	digest := sha256.Sum256([]byte(key))
	return l.namespace + ":login:" + hex.EncodeToString(digest[:])
}

func (l *RedisLimiter) Allow(ctx context.Context, key string) (bool, time.Duration, error) {
	if l == nil || l.client == nil || l.namespace == "" || l.window < time.Millisecond {
		return false, 0, errors.New("invalid Redis rate limiter configuration")
	}
	result, err := fixedWindow.Run(ctx, l.client, []string{l.redisKey(key)}, l.window.Milliseconds(), LoginLimit).Slice()
	if err != nil {
		return false, 0, fmt.Errorf("Redis rate limiter unavailable: %w", err)
	}
	if len(result) != 2 {
		return false, 0, errors.New("invalid Redis rate limiter response")
	}
	allowed, ok := result[0].(int64)
	milliseconds, ttlOK := result[1].(int64)
	if !ok || !ttlOK || (allowed != 0 && allowed != 1) || milliseconds < 0 || milliseconds > int64(l.window/time.Millisecond) {
		return false, 0, errors.New("invalid Redis rate limiter response")
	}
	return allowed == 1, time.Duration(milliseconds) * time.Millisecond, nil
}
