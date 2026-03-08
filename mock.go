//go:build !production

package redis

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// MockRedisClient implements RedisClient for testing Redis-based functionality
// It provides a simple in-memory storage for testing without requiring a real Redis instance
type MockRedisClient struct {
	mu       sync.Mutex
	data     map[string]string            // Simple in-memory storage for testing
	hashData map[string]map[string]string // Hash storage: key -> field -> value
	listData map[string][]string          // List storage: key -> ordered values
	prefix   string                       // Key prefix for consistency with PrefixedClient
}

// NewMockRedisClient creates a new mock Redis client with in-memory storage
func NewMockRedisClient() *MockRedisClient {
	return &MockRedisClient{
		data:     make(map[string]string),
		hashData: make(map[string]map[string]string),
		listData: make(map[string][]string),
		prefix:   "get-native-auth:",
	}
}

// GetPrefix returns the configured key prefix
func (m *MockRedisClient) GetPrefix() string {
	return m.prefix
}

// Get simulates Redis GET command
func (m *MockRedisClient) Get(ctx context.Context, key string) (CacheResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if value, exists := m.data[key]; exists {
		return CacheResult{Value: value, Hit: true}, nil
	}
	return CacheResult{Value: "", Hit: false}, nil
}

// GetDel simulates Redis GETDEL command (atomic get-and-delete, Redis 6.2+)
func (m *MockRedisClient) GetDel(ctx context.Context, key string) (CacheResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if value, exists := m.data[key]; exists {
		delete(m.data, key)
		return CacheResult{Value: value, Hit: true}, nil
	}
	return CacheResult{Value: "", Hit: false}, nil
}

// Set simulates Redis SET command
func (m *MockRedisClient) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.data[key] = toStringValue(value)
	return nil
}

// SetNX simulates Redis SETNX command
func (m *MockRedisClient) SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.data[key]; exists {
		return false, nil
	}

	m.data[key] = toStringValue(value)
	return true, nil
}

// Del simulates Redis DEL command (works on both string and hash keys)
func (m *MockRedisClient) Del(ctx context.Context, keys ...string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var count int64
	for _, key := range keys {
		if _, exists := m.data[key]; exists {
			delete(m.data, key)
			count++
		}
		if _, exists := m.hashData[key]; exists {
			delete(m.hashData, key)
			count++
		}
		if _, exists := m.listData[key]; exists {
			delete(m.listData, key)
			count++
		}
	}
	return count, nil
}

// Exists simulates Redis EXISTS command — checks all stores (string, list, hash)
func (m *MockRedisClient) Exists(ctx context.Context, keys ...string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var count int64
	for _, key := range keys {
		_, inData := m.data[key]
		_, inList := m.listData[key]
		_, inHash := m.hashData[key]
		if inData || inList || inHash {
			count++
		}
	}
	return count, nil
}

// Expire simulates Redis EXPIRE command — checks all stores (string, list, hash)
func (m *MockRedisClient) Expire(ctx context.Context, key string, expiration time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	_, existsInData := m.data[key]
	_, existsInList := m.listData[key]
	_, existsInHash := m.hashData[key]
	return existsInData || existsInList || existsInHash, nil
}

// TTL simulates Redis TTL command (no-op in mock)
func (m *MockRedisClient) TTL(ctx context.Context, key string) (time.Duration, error) {
	return -1, nil
}

// Keys simulates Redis KEYS command
func (m *MockRedisClient) Keys(ctx context.Context, pattern string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var keys []string
	for k := range m.data {
		keys = append(keys, k)
	}
	return keys, nil
}

// Scan simulates Redis SCAN command (not implemented for mock)
func (m *MockRedisClient) Scan(ctx context.Context, cursor uint64, match string, count int64) ScanCommand {
	return nil
}

// SAdd simulates Redis SADD command (not implemented for mock)
func (m *MockRedisClient) SAdd(ctx context.Context, key string, members ...interface{}) (int64, error) {
	return 0, nil
}

// SRem simulates Redis SREM command (not implemented for mock)
func (m *MockRedisClient) SRem(ctx context.Context, key string, members ...interface{}) (int64, error) {
	return 0, nil
}

// SMembers simulates Redis SMEMBERS command (not implemented for mock)
func (m *MockRedisClient) SMembers(ctx context.Context, key string) ([]string, error) {
	return nil, nil
}

// SCard simulates Redis SCARD command (not implemented for mock)
func (m *MockRedisClient) SCard(ctx context.Context, key string) (int64, error) {
	return 0, nil
}

// LPush simulates Redis LPUSH command — prepends values to the head of the list
func (m *MockRedisClient) LPush(ctx context.Context, key string, values ...interface{}) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, v := range values {
		m.listData[key] = append([]string{toStringValue(v)}, m.listData[key]...)
	}
	return int64(len(m.listData[key])), nil
}

// LLen simulates Redis LLEN command — returns the length of the list
func (m *MockRedisClient) LLen(ctx context.Context, key string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return int64(len(m.listData[key])), nil
}

// BRPop simulates Redis BRPOP command — pops from the tail of the first non-empty list.
// In tests this is synchronous (no blocking). Returns redis.Nil if all lists are empty.
func (m *MockRedisClient) BRPop(ctx context.Context, timeout time.Duration, keys ...string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, key := range keys {
		if list, exists := m.listData[key]; exists && len(list) > 0 {
			val := list[len(list)-1]
			m.listData[key] = list[:len(list)-1]
			if len(m.listData[key]) == 0 {
				delete(m.listData, key)
			}
			return []string{key, val}, nil
		}
	}
	return nil, redis.Nil
}

// HGet simulates Redis HGET command
func (m *MockRedisClient) HGet(ctx context.Context, key, field string) (CacheResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if hash, exists := m.hashData[key]; exists {
		if value, ok := hash[field]; ok {
			return CacheResult{Value: value, Hit: true}, nil
		}
	}
	return CacheResult{Value: "", Hit: false}, nil
}

// HSet simulates Redis HSET command
func (m *MockRedisClient) HSet(ctx context.Context, key, field string, value interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.hashData[key]; !exists {
		m.hashData[key] = make(map[string]string)
	}
	m.hashData[key][field] = toStringValue(value)
	return nil
}

// HDel simulates Redis HDEL command
func (m *MockRedisClient) HDel(ctx context.Context, key string, fields ...string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var count int64
	if hash, exists := m.hashData[key]; exists {
		for _, field := range fields {
			if _, ok := hash[field]; ok {
				delete(hash, field)
				count++
			}
		}
		// Clean up empty hashes
		if len(hash) == 0 {
			delete(m.hashData, key)
		}
	}
	return count, nil
}

// Eval simulates Redis EVAL command (not implemented for mock)
func (m *MockRedisClient) Eval(ctx context.Context, script string, keys []string, args ...interface{}) (interface{}, error) {
	return nil, nil
}

// Pipeline simulates Redis Pipeline (not implemented for mock)
func (m *MockRedisClient) Pipeline() RedisPipeliner {
	return nil
}

// Ping simulates Redis PING command
func (m *MockRedisClient) Ping(ctx context.Context) error {
	return nil
}

// Info simulates Redis INFO command
func (m *MockRedisClient) Info(ctx context.Context, section ...string) (string, error) {
	return "", nil
}

// Close simulates closing the Redis connection
func (m *MockRedisClient) Close() error {
	return nil
}

// ReadReplica returns self for mock (no replica distinction in tests)
func (m *MockRedisClient) ReadReplica() RedisClient {
	return m
}

// SetData allows tests to pre-populate the mock Redis storage
func (m *MockRedisClient) SetData(key, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
}

// GetData allows tests to inspect mock Redis storage
func (m *MockRedisClient) GetData(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, exists := m.data[key]
	return value, exists
}

// SetHashData allows tests to pre-populate mock Redis hash storage
func (m *MockRedisClient) SetHashData(key, field, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.hashData[key]; !exists {
		m.hashData[key] = make(map[string]string)
	}
	m.hashData[key][field] = value
}

// GetHashData allows tests to inspect mock Redis hash storage
func (m *MockRedisClient) GetHashData(key, field string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if hash, exists := m.hashData[key]; exists {
		value, ok := hash[field]
		return value, ok
	}
	return "", false
}

// GetHashAll allows tests to inspect all fields in a mock Redis hash
func (m *MockRedisClient) GetHashAll(key string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if hash, exists := m.hashData[key]; exists {
		// Return a copy to avoid data races
		result := make(map[string]string, len(hash))
		for k, v := range hash {
			result[k] = v
		}
		return result
	}
	return nil
}

// GetListData allows tests to inspect mock Redis list storage
func (m *MockRedisClient) GetListData(key string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if list, exists := m.listData[key]; exists {
		result := make([]string, len(list))
		copy(result, list)
		return result
	}
	return nil
}

// ClearData clears all data from mock Redis storage
func (m *MockRedisClient) ClearData() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = make(map[string]string)
	m.hashData = make(map[string]map[string]string)
	m.listData = make(map[string][]string)
}

// toStringValue converts a value to string for mock storage.
// Mirrors Redis behavior: all values are stored as strings.
func toStringValue(value interface{}) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}
