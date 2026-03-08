//go:build !production

package redis

import (
	"context"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMockRedisClient_BRPop_PopsFromTail(t *testing.T) {
	m := NewMockRedisClient()
	ctx := context.Background()

	// LPUSH prepends to head: push A, B, C → list is [C, B, A]
	_, err := m.LPush(ctx, "q", "A")
	require.NoError(t, err)
	_, err = m.LPush(ctx, "q", "B")
	require.NoError(t, err)
	_, err = m.LPush(ctx, "q", "C")
	require.NoError(t, err)

	// BRPop pops from tail → A first (FIFO with LPUSH+BRPOP)
	result, err := m.BRPop(ctx, time.Second, "q")
	require.NoError(t, err)
	assert.Equal(t, []string{"q", "A"}, result)

	result, err = m.BRPop(ctx, time.Second, "q")
	require.NoError(t, err)
	assert.Equal(t, []string{"q", "B"}, result)

	result, err = m.BRPop(ctx, time.Second, "q")
	require.NoError(t, err)
	assert.Equal(t, []string{"q", "C"}, result)
}

func TestMockRedisClient_BRPop_EmptyQueue_ReturnsRedisNil(t *testing.T) {
	m := NewMockRedisClient()
	ctx := context.Background()

	result, err := m.BRPop(ctx, time.Second, "empty-queue")
	assert.Nil(t, result)
	assert.ErrorIs(t, err, goredis.Nil)
}

func TestMockRedisClient_BRPop_CleansUpEmptyList(t *testing.T) {
	m := NewMockRedisClient()
	ctx := context.Background()

	_, err := m.LPush(ctx, "q", "only-item")
	require.NoError(t, err)

	result, err := m.BRPop(ctx, time.Second, "q")
	require.NoError(t, err)
	assert.Equal(t, []string{"q", "only-item"}, result)

	// After popping the last item, list should be cleaned up
	length, err := m.LLen(ctx, "q")
	require.NoError(t, err)
	assert.Equal(t, int64(0), length)

	// Next BRPop should return redis.Nil
	result, err = m.BRPop(ctx, time.Second, "q")
	assert.Nil(t, result)
	assert.ErrorIs(t, err, goredis.Nil)
}

func TestMockRedisClient_BRPop_MultipleKeys_PopsFirstNonEmpty(t *testing.T) {
	m := NewMockRedisClient()
	ctx := context.Background()

	// Only second key has data
	_, err := m.LPush(ctx, "q2", "value-from-q2")
	require.NoError(t, err)

	result, err := m.BRPop(ctx, time.Second, "q1", "q2")
	require.NoError(t, err)
	assert.Equal(t, []string{"q2", "value-from-q2"}, result)
}

func TestNoopClient_BRPop_ReturnsRedisNil(t *testing.T) {
	n := &noopClient{}
	result, err := n.BRPop(context.Background(), time.Second, "key")
	assert.Nil(t, result)
	assert.ErrorIs(t, err, goredis.Nil)
}

func TestLazyFallbackClient_BRPop_FactoryFails_ReturnsRedisNil(t *testing.T) {
	lazy := NewLazyFallbackClient(func() (RedisClient, error) {
		return nil, assert.AnError
	}, nil)

	result, err := lazy.BRPop(context.Background(), time.Second, "q")
	assert.Nil(t, result)
	assert.ErrorIs(t, err, goredis.Nil, "factory failure → noop → redis.Nil")
}

func TestLazyFallbackClient_BRPop_DelegatesToInner(t *testing.T) {
	mock := NewMockRedisClient()
	ctx := context.Background()

	_, err := mock.LPush(ctx, "q", "delegated-value")
	require.NoError(t, err)

	lazy := NewLazyFallbackClient(func() (RedisClient, error) {
		return mock, nil
	}, nil)

	result, err := lazy.BRPop(ctx, time.Second, "q")
	require.NoError(t, err)
	assert.Equal(t, []string{"q", "delegated-value"}, result)
}
