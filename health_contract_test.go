package redis

import (
	"testing"

	"github.com/redis/go-redis/v9"
)

// TestGoRedisFailoverClientContract verifies the go-redis library contract
// that CheckHealth() depends on for Sentinel mode detection.
//
// This test validates that go-redis v9 sets Addr="FailoverClient" when creating
// a Sentinel client via NewFailoverClient(). If this test fails after a go-redis
// upgrade, the Sentinel detection logic in CheckHealth() must be updated.
//
// Contract location: vendor/github.com/redis/go-redis/v9/sentinel.go:122
// Detection logic: pkg/redis/health.go CheckHealth() function
func TestGoRedisFailoverClientContract(t *testing.T) {
	// Create a FailoverClient (Sentinel client)
	// We don't need actual Sentinel connectivity for this test - just need to
	// verify that the client configuration is set correctly
	failoverOpts := &redis.FailoverOptions{
		MasterName:    "test-master",
		SentinelAddrs: []string{"localhost:26379"},
	}

	client := redis.NewFailoverClient(failoverOpts)
	defer client.Close()

	// Get client options to inspect the contract
	opts := client.Options()

	// CRITICAL CONTRACT CHECK: go-redis sets Addr="FailoverClient" as a marker
	// for Sentinel clients at vendor/github.com/redis/go-redis/v9/sentinel.go:122
	if opts.Addr != "FailoverClient" {
		t.Fatalf(
			"go-redis contract CHANGED: expected Addr='FailoverClient', got '%s'\n\n"+
				"This detection relies on vendor/github.com/redis/go-redis/v9/sentinel.go:122\n"+
				"where FailoverOptions.clientOptions() sets Addr=\"FailoverClient\".\n\n"+
				"ACTION REQUIRED:\n"+
				"1. Review go-redis changelog for Sentinel client changes\n"+
				"2. Update Sentinel detection logic in pkg/redis/health.go CheckHealth()\n"+
				"3. Consider using INFO replication parsing as fallback\n"+
				"4. Update this test with new contract expectations\n",
			opts.Addr,
		)
	}

	t.Logf("✓ go-redis contract verified: Addr='FailoverClient' for Sentinel clients")
}

// TestStandaloneClientContract verifies that standalone clients have a real address
// This helps ensure our detection logic can distinguish between standalone and Sentinel
func TestStandaloneClientContract(t *testing.T) {
	// Create a standalone client
	standaloneOpts := &redis.Options{
		Addr: "localhost:6379",
	}

	client := redis.NewClient(standaloneOpts)
	defer client.Close()

	// Get client options
	opts := client.Options()

	// Standalone clients should have their actual address, not "FailoverClient"
	if opts.Addr == "FailoverClient" {
		t.Fatalf(
			"Unexpected: standalone client has Addr='FailoverClient'\n"+
				"This would cause false positive Sentinel detection\n"+
				"Actual Addr: %s",
			opts.Addr,
		)
	}

	if opts.Addr != "localhost:6379" {
		t.Fatalf(
			"Standalone client address mismatch\n"+
				"Expected: localhost:6379\n"+
				"Got: %s",
			opts.Addr,
		)
	}

	t.Logf("✓ Standalone client contract verified: Addr='%s' (not 'FailoverClient')", opts.Addr)
}
