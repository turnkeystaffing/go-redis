package redis

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// LazyFallbackClient defers Redis connection until the first method call.
// Used in CLI mode: commands that never touch Redis pay zero connection cost.
// If the factory fails on first use, all subsequent calls get noop behavior
// (cache misses, silent no-ops) — same as if Redis were disabled.
type LazyFallbackClient struct {
	once    sync.Once
	inner   RedisClient
	factory func() (RedisClient, error)
	logWarn func(msg string) // log warning on fallback (nil-safe)
}

// NewLazyFallbackClient creates a client that connects Redis on first method call.
// The factory should create the real PrefixedClient (connect + ping).
// logWarn is called once if the factory fails (pass nil to suppress).
func NewLazyFallbackClient(factory func() (RedisClient, error), logWarn func(string)) *LazyFallbackClient {
	return &LazyFallbackClient{factory: factory, logWarn: logWarn}
}

func (l *LazyFallbackClient) resolve() RedisClient {
	l.once.Do(func() {
		client, err := l.factory()
		if err != nil {
			if l.logWarn != nil {
				l.logWarn("Redis unavailable, using noop client: " + err.Error())
			}
			l.inner = &noopClient{}
			return
		}
		l.inner = client
	})
	return l.inner
}

// Close cleans up Redis. Uses once.Do to avoid a data race on l.inner:
// if already resolved, once.Do is a no-op and we close the real client;
// if never resolved, once.Do sets noop (factory never called) and Close is a no-op.
func (l *LazyFallbackClient) Close() error {
	l.once.Do(func() { l.inner = &noopClient{} })
	return l.inner.Close()
}

// --- RedisClient interface delegation (one-liner per method) ---

func (l *LazyFallbackClient) GetPrefix() string { return l.resolve().GetPrefix() }
func (l *LazyFallbackClient) Get(ctx context.Context, key string) (CacheResult, error) {
	return l.resolve().Get(ctx, key)
}
func (l *LazyFallbackClient) GetDel(ctx context.Context, key string) (CacheResult, error) {
	return l.resolve().GetDel(ctx, key)
}
func (l *LazyFallbackClient) Set(ctx context.Context, key string, value interface{}, exp time.Duration) error {
	return l.resolve().Set(ctx, key, value, exp)
}
func (l *LazyFallbackClient) SetNX(ctx context.Context, key string, value interface{}, exp time.Duration) (bool, error) {
	return l.resolve().SetNX(ctx, key, value, exp)
}
func (l *LazyFallbackClient) Del(ctx context.Context, keys ...string) (int64, error) {
	return l.resolve().Del(ctx, keys...)
}
func (l *LazyFallbackClient) Exists(ctx context.Context, keys ...string) (int64, error) {
	return l.resolve().Exists(ctx, keys...)
}
func (l *LazyFallbackClient) Expire(ctx context.Context, key string, exp time.Duration) (bool, error) {
	return l.resolve().Expire(ctx, key, exp)
}
func (l *LazyFallbackClient) TTL(ctx context.Context, key string) (time.Duration, error) {
	return l.resolve().TTL(ctx, key)
}
func (l *LazyFallbackClient) Keys(ctx context.Context, key string) ([]string, error) {
	return l.resolve().Keys(ctx, key)
}
func (l *LazyFallbackClient) Scan(ctx context.Context, cursor uint64, match string, count int64) ScanCommand {
	return l.resolve().Scan(ctx, cursor, match, count)
}
func (l *LazyFallbackClient) HGet(ctx context.Context, key, field string) (CacheResult, error) {
	return l.resolve().HGet(ctx, key, field)
}
func (l *LazyFallbackClient) HSet(ctx context.Context, key, field string, value interface{}) error {
	return l.resolve().HSet(ctx, key, field, value)
}
func (l *LazyFallbackClient) HDel(ctx context.Context, key string, fields ...string) (int64, error) {
	return l.resolve().HDel(ctx, key, fields...)
}
func (l *LazyFallbackClient) SAdd(ctx context.Context, key string, members ...interface{}) (int64, error) {
	return l.resolve().SAdd(ctx, key, members...)
}
func (l *LazyFallbackClient) SRem(ctx context.Context, key string, members ...interface{}) (int64, error) {
	return l.resolve().SRem(ctx, key, members...)
}
func (l *LazyFallbackClient) SMembers(ctx context.Context, key string) ([]string, error) {
	return l.resolve().SMembers(ctx, key)
}
func (l *LazyFallbackClient) SCard(ctx context.Context, key string) (int64, error) {
	return l.resolve().SCard(ctx, key)
}
func (l *LazyFallbackClient) LPush(ctx context.Context, key string, values ...interface{}) (int64, error) {
	return l.resolve().LPush(ctx, key, values...)
}
func (l *LazyFallbackClient) LLen(ctx context.Context, key string) (int64, error) {
	return l.resolve().LLen(ctx, key)
}
func (l *LazyFallbackClient) BRPop(ctx context.Context, timeout time.Duration, keys ...string) ([]string, error) {
	return l.resolve().BRPop(ctx, timeout, keys...)
}
func (l *LazyFallbackClient) Eval(ctx context.Context, script string, keys []string, args ...interface{}) (interface{}, error) {
	return l.resolve().Eval(ctx, script, keys, args...)
}
func (l *LazyFallbackClient) Pipeline() RedisPipeliner       { return l.resolve().Pipeline() }
func (l *LazyFallbackClient) Ping(ctx context.Context) error { return l.resolve().Ping(ctx) }
func (l *LazyFallbackClient) Info(ctx context.Context, section ...string) (string, error) {
	return l.resolve().Info(ctx, section...)
}
func (l *LazyFallbackClient) ReadReplica() RedisClient { return l.resolve().ReadReplica() }

// Compile-time check
var _ RedisClient = (*LazyFallbackClient)(nil)

// NewNoopClient returns a RedisClient that silently no-ops all operations.
// Use as a default when Redis is not configured — eliminates nil checks on consumers.
func NewNoopClient() RedisClient { return &noopClient{} }

// =============================================================================
// noopClient — silent fallback when Redis is unavailable
// =============================================================================

// noopClient returns zero values for all operations (cache misses, no-ops).
// Used as the fallback when lazy Redis connection fails in CLI mode.
type noopClient struct{}

func (n *noopClient) GetPrefix() string                                             { return "" }
func (n *noopClient) Get(context.Context, string) (CacheResult, error)              { return CacheResult{}, nil }
func (n *noopClient) GetDel(context.Context, string) (CacheResult, error)           { return CacheResult{}, nil }
func (n *noopClient) Set(context.Context, string, interface{}, time.Duration) error { return nil }
func (n *noopClient) SetNX(context.Context, string, interface{}, time.Duration) (bool, error) {
	return true, nil
}
func (n *noopClient) Del(context.Context, ...string) (int64, error)               { return 0, nil }
func (n *noopClient) Exists(context.Context, ...string) (int64, error)            { return 0, nil }
func (n *noopClient) Expire(context.Context, string, time.Duration) (bool, error) { return false, nil }
func (n *noopClient) TTL(context.Context, string) (time.Duration, error)          { return 0, nil }
func (n *noopClient) Keys(context.Context, string) ([]string, error)              { return nil, nil }
func (n *noopClient) Scan(context.Context, uint64, string, int64) ScanCommand     { return &noopScanCmd{} }
func (n *noopClient) HGet(context.Context, string, string) (CacheResult, error) {
	return CacheResult{}, nil
}
func (n *noopClient) HSet(context.Context, string, string, interface{}) error      { return nil }
func (n *noopClient) HDel(context.Context, string, ...string) (int64, error)       { return 0, nil }
func (n *noopClient) SAdd(context.Context, string, ...interface{}) (int64, error)  { return 0, nil }
func (n *noopClient) SRem(context.Context, string, ...interface{}) (int64, error)  { return 0, nil }
func (n *noopClient) SMembers(context.Context, string) ([]string, error)           { return nil, nil }
func (n *noopClient) SCard(context.Context, string) (int64, error)                 { return 0, nil }
func (n *noopClient) LPush(context.Context, string, ...interface{}) (int64, error) { return 0, nil }
func (n *noopClient) LLen(context.Context, string) (int64, error)                  { return 0, nil }
func (n *noopClient) BRPop(context.Context, time.Duration, ...string) ([]string, error) {
	return nil, redis.Nil
}
func (n *noopClient) Eval(context.Context, string, []string, ...interface{}) (interface{}, error) {
	return nil, nil
}
func (n *noopClient) Pipeline() RedisPipeliner                        { return &noopPipeliner{} }
func (n *noopClient) Ping(context.Context) error                      { return nil }
func (n *noopClient) Info(context.Context, ...string) (string, error) { return "", nil }
func (n *noopClient) Close() error                                    { return nil }
func (n *noopClient) ReadReplica() RedisClient                        { return n }

var _ RedisClient = (*noopClient)(nil)

// --- Noop command types for Scan and Pipeline ---

type noopScanCmd struct{}

func (noopScanCmd) Val() ([]string, uint64)           { return nil, 0 }
func (noopScanCmd) Result() ([]string, uint64, error) { return nil, 0, nil }

type noopPipeliner struct{}

func (*noopPipeliner) Incr(_ context.Context, _ string) IntCommand { return &noopIntCmd{} }
func (*noopPipeliner) Expire(_ context.Context, _ string, _ time.Duration) BoolCommand {
	return &noopBoolCmd{}
}
func (*noopPipeliner) Exec(context.Context) error { return nil }

type noopIntCmd struct{}

func (*noopIntCmd) Val() int64             { return 0 }
func (*noopIntCmd) Result() (int64, error) { return 0, nil }

type noopBoolCmd struct{}

func (*noopBoolCmd) Val() bool             { return false }
func (*noopBoolCmd) Result() (bool, error) { return false, nil }
