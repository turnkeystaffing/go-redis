package redis

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClient is a minimal RedisClient that tracks calls and returns configured values.
type fakeClient struct {
	noopClient              // embed noop for all unimplemented methods
	prefix     string       // custom prefix
	getCalled  atomic.Int32 // tracks Get calls
	closed     atomic.Bool  // tracks Close
}

func (f *fakeClient) GetPrefix() string { return f.prefix }
func (f *fakeClient) Get(ctx context.Context, key string) (CacheResult, error) {
	f.getCalled.Add(1)
	return CacheResult{Value: "test-value", Hit: true}, nil
}
func (f *fakeClient) Set(ctx context.Context, key string, value interface{}, exp time.Duration) error {
	return nil
}
func (f *fakeClient) Ping(ctx context.Context) error { return nil }
func (f *fakeClient) Close() error {
	f.closed.Store(true)
	return nil
}

func TestLazyFallbackClient_FactoryCalledOnFirstUse(t *testing.T) {
	var factoryCalls atomic.Int32
	fake := &fakeClient{prefix: "test:"}

	client := NewLazyFallbackClient(func() (RedisClient, error) {
		factoryCalls.Add(1)
		return fake, nil
	}, nil)

	// Factory not called yet
	assert.Equal(t, int32(0), factoryCalls.Load())

	// First method call triggers factory
	prefix := client.GetPrefix()
	assert.Equal(t, "test:", prefix)
	assert.Equal(t, int32(1), factoryCalls.Load())

	// Second call reuses the same client (factory NOT called again)
	prefix = client.GetPrefix()
	assert.Equal(t, "test:", prefix)
	assert.Equal(t, int32(1), factoryCalls.Load())
}

func TestLazyFallbackClient_DelegatesToRealClient(t *testing.T) {
	fake := &fakeClient{prefix: "real:"}

	client := NewLazyFallbackClient(func() (RedisClient, error) {
		return fake, nil
	}, nil)

	ctx := context.Background()

	// Get delegates to fake
	result, err := client.Get(ctx, "key1")
	require.NoError(t, err)
	assert.True(t, result.Hit)
	assert.Equal(t, "test-value", result.Value)
	assert.Equal(t, int32(1), fake.getCalled.Load())

	// Multiple calls all delegate
	_, _ = client.Get(ctx, "key2")
	assert.Equal(t, int32(2), fake.getCalled.Load())
}

func TestLazyFallbackClient_NoopFallbackOnFactoryError(t *testing.T) {
	client := NewLazyFallbackClient(func() (RedisClient, error) {
		return nil, errors.New("connection refused")
	}, nil)

	ctx := context.Background()

	// Get returns cache miss (noop behavior)
	result, err := client.Get(ctx, "key")
	require.NoError(t, err)
	assert.False(t, result.Hit)
	assert.Empty(t, result.Value)

	// SetNX returns true (optimistic lock acquisition)
	ok, err := client.SetNX(ctx, "lock", "val", time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)

	// Set is silent no-op
	err = client.Set(ctx, "key", "val", time.Minute)
	require.NoError(t, err)

	// Del returns 0
	n, err := client.Del(ctx, "key")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// Exists returns 0
	n, err = client.Exists(ctx, "key")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// LPush returns 0
	n, err = client.LPush(ctx, "list", "val1", "val2")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// LLen returns 0
	n, err = client.LLen(ctx, "list")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// Ping is no-op
	err = client.Ping(ctx)
	require.NoError(t, err)

	// Info returns empty
	info, err := client.Info(ctx)
	require.NoError(t, err)
	assert.Empty(t, info)

	// Pipeline returns noop pipeliner
	pipe := client.Pipeline()
	require.NotNil(t, pipe)
	cmd := pipe.Incr(ctx, "counter")
	require.NotNil(t, cmd)
	assert.Equal(t, int64(0), cmd.Val())
	err = pipe.Exec(ctx)
	require.NoError(t, err)
}

func TestLazyFallbackClient_CloseWithoutInit(t *testing.T) {
	var factoryCalled atomic.Bool

	client := NewLazyFallbackClient(func() (RedisClient, error) {
		factoryCalled.Store(true)
		return &fakeClient{}, nil
	}, nil)

	// Close before any method call — should NOT trigger factory
	err := client.Close()
	require.NoError(t, err)
	assert.False(t, factoryCalled.Load(), "factory should not be called on Close()")
}

func TestLazyFallbackClient_CloseAfterInit(t *testing.T) {
	fake := &fakeClient{}
	client := NewLazyFallbackClient(func() (RedisClient, error) {
		return fake, nil
	}, nil)

	// Trigger init
	_ = client.GetPrefix()

	// Close delegates to real client
	err := client.Close()
	require.NoError(t, err)
	assert.True(t, fake.closed.Load(), "real client should be closed")
}

func TestLazyFallbackClient_WarningLoggedOnFallback(t *testing.T) {
	var warnings []string
	var mu sync.Mutex

	client := NewLazyFallbackClient(func() (RedisClient, error) {
		return nil, errors.New("dial tcp: connection refused")
	}, func(msg string) {
		mu.Lock()
		warnings = append(warnings, msg)
		mu.Unlock()
	})

	// Trigger fallback
	_ = client.GetPrefix()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "connection refused")
	assert.Contains(t, warnings[0], "noop client")
}

func TestLazyFallbackClient_NilLogWarnSafe(t *testing.T) {
	client := NewLazyFallbackClient(func() (RedisClient, error) {
		return nil, errors.New("fail")
	}, nil) // nil logWarn

	// Should not panic
	assert.NotPanics(t, func() {
		_ = client.GetPrefix()
	})
}

func TestLazyFallbackClient_ConcurrentAccess(t *testing.T) {
	var factoryCalls atomic.Int32
	fake := &fakeClient{prefix: "concurrent:"}

	client := NewLazyFallbackClient(func() (RedisClient, error) {
		factoryCalls.Add(1)
		return fake, nil
	}, nil)

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			prefix := client.GetPrefix()
			assert.Equal(t, "concurrent:", prefix)
		}()
	}

	wg.Wait()

	// Factory called exactly once despite concurrent access
	assert.Equal(t, int32(1), factoryCalls.Load())
}

func TestLazyFallbackClient_ScanReturnsNoopOnFallback(t *testing.T) {
	client := NewLazyFallbackClient(func() (RedisClient, error) {
		return nil, errors.New("fail")
	}, nil)

	cmd := client.Scan(context.Background(), 0, "*", 100)
	keys, cursor := cmd.Val()
	assert.Nil(t, keys)
	assert.Equal(t, uint64(0), cursor)

	keys, cursor, err := cmd.Result()
	require.NoError(t, err)
	assert.Nil(t, keys)
	assert.Equal(t, uint64(0), cursor)
}

func TestLazyFallbackClient_HashOpsNoopOnFallback(t *testing.T) {
	client := NewLazyFallbackClient(func() (RedisClient, error) {
		return nil, errors.New("fail")
	}, nil)

	ctx := context.Background()

	result, err := client.HGet(ctx, "hash", "field")
	require.NoError(t, err)
	assert.False(t, result.Hit)

	err = client.HSet(ctx, "hash", "field", "value")
	require.NoError(t, err)

	n, err := client.HDel(ctx, "hash", "field")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}

func TestLazyFallbackClient_SetOpsNoopOnFallback(t *testing.T) {
	client := NewLazyFallbackClient(func() (RedisClient, error) {
		return nil, errors.New("fail")
	}, nil)

	ctx := context.Background()

	n, err := client.SAdd(ctx, "set", "member1")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	n, err = client.SRem(ctx, "set", "member1")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	members, err := client.SMembers(ctx, "set")
	require.NoError(t, err)
	assert.Nil(t, members)

	n, err = client.SCard(ctx, "set")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}

func TestLazyFallbackClient_ReadReplicaNoopOnFallback(t *testing.T) {
	client := NewLazyFallbackClient(func() (RedisClient, error) {
		return nil, errors.New("fail")
	}, nil)

	replica := client.ReadReplica()
	require.NotNil(t, replica)
	// Noop ReadReplica returns itself
	assert.Equal(t, replica, replica.ReadReplica())
}

func TestLazyFallbackClient_CompileTimeCheck(t *testing.T) {
	// Verify interface compliance (also checked at package level, but explicit in test)
	var _ RedisClient = (*LazyFallbackClient)(nil)
}

// TestNoopClient_Semantics verifies the noop client's contract:
// reads return misses, writes succeed silently, SetNX is optimistic.
func TestNoopClient_Semantics(t *testing.T) {
	var c noopClient
	ctx := context.Background()

	t.Run("Get returns cache miss", func(t *testing.T) {
		r, err := c.Get(ctx, "any")
		require.NoError(t, err)
		assert.False(t, r.Hit)
	})

	t.Run("GetDel returns cache miss", func(t *testing.T) {
		r, err := c.GetDel(ctx, "any")
		require.NoError(t, err)
		assert.False(t, r.Hit)
	})

	t.Run("SetNX returns true (optimistic)", func(t *testing.T) {
		ok, err := c.SetNX(ctx, "lock", "val", time.Minute)
		require.NoError(t, err)
		assert.True(t, ok, "noop SetNX should be optimistic for lock acquisition")
	})

	t.Run("Exists returns 0", func(t *testing.T) {
		n, err := c.Exists(ctx, "key")
		require.NoError(t, err)
		assert.Equal(t, int64(0), n)
	})

	t.Run("TTL returns 0", func(t *testing.T) {
		ttl, err := c.TTL(ctx, "key")
		require.NoError(t, err)
		assert.Equal(t, time.Duration(0), ttl)
	})

	t.Run("LPush returns 0", func(t *testing.T) {
		n, err := c.LPush(ctx, "list", "val")
		require.NoError(t, err)
		assert.Equal(t, int64(0), n)
	})

	t.Run("LLen returns 0", func(t *testing.T) {
		n, err := c.LLen(ctx, "list")
		require.NoError(t, err)
		assert.Equal(t, int64(0), n)
	})

	t.Run("Eval returns nil", func(t *testing.T) {
		result, err := c.Eval(ctx, "script", nil)
		require.NoError(t, err)
		assert.Nil(t, result)
	})

	t.Run("Close is no-op", func(t *testing.T) {
		err := c.Close()
		require.NoError(t, err)
	})
}
