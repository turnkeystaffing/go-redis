package redis

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// CacheResult represents the result of a cache lookup operation
// Provides clean cache semantics without allocation overhead or implementation leakage
type CacheResult struct {
	Value string // The cached value (empty string if cache miss)
	Hit   bool   // true if value was found in cache, false for cache miss
}

// RedisClient defines the minimal interface we actually need
// This allows easy mocking and doesn't depend on the massive redis.UniversalClient interface
type RedisClient interface {
	GetPrefix() string
	// Key-value operations
	Get(ctx context.Context, key string) (CacheResult, error)
	GetDel(ctx context.Context, key string) (CacheResult, error) // Atomic get-and-delete (Lua script, Redis 6.0+)
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error
	SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) (bool, error)
	Del(ctx context.Context, keys ...string) (int64, error)
	Exists(ctx context.Context, keys ...string) (int64, error)
	Expire(ctx context.Context, key string, expiration time.Duration) (bool, error)
	TTL(ctx context.Context, key string) (time.Duration, error)
	Keys(ctx context.Context, key string) ([]string, error)
	Scan(ctx context.Context, cursor uint64, match string, count int64) ScanCommand

	// Hash operations
	HGet(ctx context.Context, key, field string) (CacheResult, error)
	HSet(ctx context.Context, key, field string, value interface{}) error
	HDel(ctx context.Context, key string, fields ...string) (int64, error)

	// Set operations
	SAdd(ctx context.Context, key string, members ...interface{}) (int64, error)
	SRem(ctx context.Context, key string, members ...interface{}) (int64, error)
	SMembers(ctx context.Context, key string) ([]string, error)
	SCard(ctx context.Context, key string) (int64, error)

	// List operations
	LPush(ctx context.Context, key string, values ...interface{}) (int64, error)
	LLen(ctx context.Context, key string) (int64, error)
	BRPop(ctx context.Context, timeout time.Duration, keys ...string) ([]string, error)

	// Script execution (for distributed locks)
	Eval(ctx context.Context, script string, keys []string, args ...interface{}) (interface{}, error)

	// Pipeline operations
	Pipeline() RedisPipeliner

	// Infrastructure
	Ping(ctx context.Context) error
	Info(ctx context.Context, section ...string) (string, error)
	Close() error

	// ReadReplica returns a client that reads from Sentinel replicas instead of master.
	// Use ONLY for eventually-consistent reads where stale data is acceptable:
	// - Cache lookups with fail-open semantics (BFF token cache)
	// - Informational/display data (user session listing)
	// - Admin/metrics queries (session count, memory info)
	// DO NOT use for security-critical reads (blacklist checks, session validation, CSRF, PKCE).
	// Returns self (master client) when no replica is configured (standalone mode or disabled).
	ReadReplica() RedisClient
}

// IntCommand represents a deferred integer command result
// This abstracts away the Redis library's IntCmd implementation
type IntCommand interface {
	Val() int64
	Result() (int64, error)
}

// BoolCommand represents a deferred boolean command result
// This abstracts away the Redis library's BoolCmd implementation
type BoolCommand interface {
	Val() bool
	Result() (bool, error)
}

// ScanCommand represents a deferred scan command result
// This abstracts away the Redis library's ScanCmd implementation
type ScanCommand interface {
	Val() (keys []string, cursor uint64)
	Result() (keys []string, cursor uint64, err error)
}

// RedisPipeliner defines the minimal pipeliner interface we actually need
// This allows easy mocking and doesn't depend on the massive redis.Pipeliner interface
type RedisPipeliner interface {
	// Incr Pipeline commands - return command objects for deferred execution
	// Results must be retrieved AFTER calling Exec()
	Incr(ctx context.Context, key string) IntCommand
	Expire(ctx context.Context, key string, expiration time.Duration) BoolCommand
	// Exec Pipeline execution - returns only error since command results are retrieved
	// from the command objects returned by Incr/Expire methods
	Exec(ctx context.Context) error
}

// PrefixedClient wraps any RedisClient implementation with automatic key prefixing
// Uses composition and dependency injection instead of wrapping the huge UniversalClient
type PrefixedClient struct {
	client  redis.UniversalClient
	replica *PrefixedClient // optional read-replica client; nil = use master for all reads
	prefix  string
}

// NewPrefixedClient creates a new client with automatic key prefixing via DI
func NewPrefixedClient(client redis.UniversalClient, prefix string) *PrefixedClient {
	if prefix == "" {
		prefix = "get-native-auth:"
	}
	return &PrefixedClient{
		client: client,
		prefix: prefix,
	}
}

// SetReplicaClient attaches an optional read-replica client for Sentinel replica reads.
// When set, ReadReplica() returns a PrefixedClient wrapping this replica connection.
// Pass nil to disable replica reads (all reads go to master).
func (p *PrefixedClient) SetReplicaClient(replicaConn redis.UniversalClient) {
	if replicaConn == nil {
		p.replica = nil
		return
	}
	p.replica = &PrefixedClient{
		client: replicaConn,
		prefix: p.prefix,
		// replica's own replica is nil — no chaining
	}
}

// ReadReplica returns a client that reads from Sentinel replicas.
// Falls back to self (master) when no replica is configured.
func (p *PrefixedClient) ReadReplica() RedisClient {
	if p.replica != nil {
		return p.replica
	}
	return p
}

// GetPrefix returns the configured key prefix
func (p *PrefixedClient) GetPrefix() string {
	return p.prefix
}

// prefixKey adds the prefix to a key
func (p *PrefixedClient) prefixKey(key string) string {
	return p.prefix + key
}

// prefixKeys adds the prefix to multiple keys
func (p *PrefixedClient) prefixKeys(keys []string) []string {
	prefixed := make([]string, len(keys))
	for i, key := range keys {
		prefixed[i] = p.prefixKey(key)
	}
	return prefixed
}

// --- RedisClient interface implementation with prefixing ---

func (p *PrefixedClient) Get(ctx context.Context, key string) (CacheResult, error) {
	value, err := p.client.Get(ctx, p.prefixKey(key)).Result()
	if err != nil {
		// Cache miss is not an error - it's a normal case
		if errors.Is(err, redis.Nil) {
			return CacheResult{Value: "", Hit: false}, nil
		}
		// Real error (network, Redis down, etc.)
		return CacheResult{Value: "", Hit: false}, err
	}
	// Cache hit
	return CacheResult{Value: value, Hit: true}, nil
}

// getDelScript is a Lua script that atomically gets and deletes a key.
// Compatible with Redis 6.0+ (unlike GETDEL which requires Redis 6.2+).
// Lua scripts execute atomically on the Redis server - no other command
// can run between the GET and DEL, providing the same guarantees as GETDEL.
const getDelScript = `local val = redis.call("GET", KEYS[1])
if val then
	redis.call("DEL", KEYS[1])
end
return val`

// GetDel atomically gets and deletes a key using a Lua script.
// This is essential for one-time use tokens like PKCE verifiers.
// Uses Lua script instead of native GETDEL for Redis 6.0 compatibility.
func (p *PrefixedClient) GetDel(ctx context.Context, key string) (CacheResult, error) {
	prefixedKey := p.prefixKey(key)
	result, err := p.client.Eval(ctx, getDelScript, []string{prefixedKey}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return CacheResult{Value: "", Hit: false}, nil
		}
		return CacheResult{Value: "", Hit: false}, err
	}
	// Lua returns nil (go-redis maps to redis.Nil) when key doesn't exist,
	// but if we reach here, result is non-nil
	if result == nil {
		return CacheResult{Value: "", Hit: false}, nil
	}
	value, ok := result.(string)
	if !ok {
		return CacheResult{Value: "", Hit: false}, nil
	}
	return CacheResult{Value: value, Hit: true}, nil
}

func (p *PrefixedClient) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	return p.client.Set(ctx, p.prefixKey(key), value, expiration).Err()
}

func (p *PrefixedClient) SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) (bool, error) {
	return p.client.SetNX(ctx, p.prefixKey(key), value, expiration).Result()
}

func (p *PrefixedClient) Del(ctx context.Context, keys ...string) (int64, error) {
	return p.client.Del(ctx, p.prefixKeys(keys)...).Result()
}

func (p *PrefixedClient) Exists(ctx context.Context, keys ...string) (int64, error) {
	return p.client.Exists(ctx, p.prefixKeys(keys)...).Result()
}

func (p *PrefixedClient) Expire(ctx context.Context, key string, expiration time.Duration) (bool, error) {
	return p.client.Expire(ctx, p.prefixKey(key), expiration).Result()
}

func (p *PrefixedClient) TTL(ctx context.Context, key string) (time.Duration, error) {
	return p.client.TTL(ctx, p.prefixKey(key)).Result()
}

func (p *PrefixedClient) Keys(ctx context.Context, key string) ([]string, error) {
	return p.client.Keys(ctx, p.prefixKey(key)).Result()
}

func (p *PrefixedClient) Scan(ctx context.Context, cursor uint64, match string, count int64) ScanCommand {
	return p.client.Scan(ctx, cursor, p.prefixKey(match), count)
}

func (p *PrefixedClient) HGet(ctx context.Context, key, field string) (CacheResult, error) {
	value, err := p.client.HGet(ctx, p.prefixKey(key), field).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return CacheResult{Value: "", Hit: false}, nil
		}
		return CacheResult{Value: "", Hit: false}, err
	}
	return CacheResult{Value: value, Hit: true}, nil
}

func (p *PrefixedClient) HSet(ctx context.Context, key, field string, value interface{}) error {
	return p.client.HSet(ctx, p.prefixKey(key), field, value).Err()
}

func (p *PrefixedClient) HDel(ctx context.Context, key string, fields ...string) (int64, error) {
	return p.client.HDel(ctx, p.prefixKey(key), fields...).Result()
}

func (p *PrefixedClient) SAdd(ctx context.Context, key string, members ...interface{}) (int64, error) {
	return p.client.SAdd(ctx, p.prefixKey(key), members...).Result()
}

func (p *PrefixedClient) SRem(ctx context.Context, key string, members ...interface{}) (int64, error) {
	return p.client.SRem(ctx, p.prefixKey(key), members...).Result()
}

func (p *PrefixedClient) SMembers(ctx context.Context, key string) ([]string, error) {
	return p.client.SMembers(ctx, p.prefixKey(key)).Result()
}

func (p *PrefixedClient) SCard(ctx context.Context, key string) (int64, error) {
	return p.client.SCard(ctx, p.prefixKey(key)).Result()
}

func (p *PrefixedClient) LPush(ctx context.Context, key string, values ...interface{}) (int64, error) {
	return p.client.LPush(ctx, p.prefixKey(key), values...).Result()
}

func (p *PrefixedClient) LLen(ctx context.Context, key string) (int64, error) {
	return p.client.LLen(ctx, p.prefixKey(key)).Result()
}

func (p *PrefixedClient) BRPop(ctx context.Context, timeout time.Duration, keys ...string) ([]string, error) {
	result, err := p.client.BRPop(ctx, timeout, p.prefixKeys(keys)...).Result()
	if err != nil {
		return nil, err
	}
	// Strip prefix from returned key name (result[0])
	if len(result) > 0 {
		for _, key := range keys {
			if result[0] == p.prefixKey(key) {
				result[0] = key
				break
			}
		}
	}
	return result, nil
}

func (p *PrefixedClient) Eval(ctx context.Context, script string, keys []string, args ...interface{}) (interface{}, error) {
	return p.client.Eval(ctx, script, p.prefixKeys(keys), args...).Result()
}

func (p *PrefixedClient) Pipeline() RedisPipeliner {
	// Return prefixed pipeliner
	return &PrefixedPipeliner{
		Pipeliner: p.client.Pipeline(),
		prefix:    p.prefix,
	}
}

func (p *PrefixedClient) Ping(ctx context.Context) error {
	return p.client.Ping(ctx).Err()
}

func (p *PrefixedClient) Info(ctx context.Context, section ...string) (string, error) {
	// Info is a server-level command, no key prefixing needed
	return p.client.Info(ctx, section...).Result()
}

func (p *PrefixedClient) Close() error {
	return p.client.Close()
}

// PrefixedPipeliner wraps any RedisPipeliner with automatic key prefixing using DI
type PrefixedPipeliner struct {
	Pipeliner redis.Pipeliner
	prefix    string
}

func (p *PrefixedPipeliner) prefixKey(key string) string {
	return p.prefix + key
}

func (p *PrefixedPipeliner) prefixKeys(keys []string) []string {
	prefixed := make([]string, len(keys))
	for i, key := range keys {
		prefixed[i] = p.prefixKey(key)
	}
	return prefixed
}

// Incr increments the integer value of a key (used in rate limiting)
// Returns a command object for deferred execution - call .Val() or .Result() after Exec()
func (p *PrefixedPipeliner) Incr(ctx context.Context, key string) IntCommand {
	return p.Pipeliner.Incr(ctx, p.prefixKey(key))
}

// Expire sets expiration on a key
// Returns a command object for deferred execution - call .Val() or .Result() after Exec()
func (p *PrefixedPipeliner) Expire(ctx context.Context, key string, expiration time.Duration) BoolCommand {
	return p.Pipeliner.Expire(ctx, p.prefixKey(key), expiration)
}

func (p *PrefixedPipeliner) Exec(ctx context.Context) error {
	_, err := p.Pipeliner.Exec(ctx)
	return err
}

// Ensure adapters implement our interfaces
var _ RedisClient = (*PrefixedClient)(nil)
var _ RedisPipeliner = (*PrefixedPipeliner)(nil)

// Ensure Redis library types satisfy our command interfaces
// This compile-time check verifies that *redis.IntCmd, *redis.BoolCmd, and *redis.ScanCmd
// automatically satisfy our IntCommand, BoolCommand, and ScanCommand interfaces through
// structural typing (they have the required Val() and Result() methods)
var _ IntCommand = (*redis.IntCmd)(nil)
var _ BoolCommand = (*redis.BoolCmd)(nil)
var _ ScanCommand = (*redis.ScanCmd)(nil)
