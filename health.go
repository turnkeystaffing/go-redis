package redis

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// HealthCheckInfo holds comprehensive health check results
// Adapts to both standalone and Sentinel configurations
type HealthCheckInfo struct {
	IsHealthy         bool
	BasicConnectivity bool
	Mode              string // "standalone" or "sentinel"

	// Sentinel-specific fields (empty in standalone mode)
	MasterName       string
	ConnectedSlaves  int
	MasterReplOffset int64

	// Replica client health (only populated when sentinel_read_replica is enabled)
	ReplicaConfigured bool // true if a read-replica client is attached
	ReplicaHealthy    bool // true if replica ping succeeded

	ErrorMessage string
}

// CheckHealth performs unified health check that adapts to client type
// Detects Sentinel vs standalone by inspecting the underlying redis.Client configuration
//
// CONTRACT DEPENDENCY: This detection relies on go-redis v9 setting
// Addr="FailoverClient" for Sentinel clients (sentinel.go:122).
// If go-redis changes this behavior:
//  1. Unit test TestGoRedisFailoverClientContract will fail
//  2. Update detection logic or use fallback INFO parsing
//  3. See go.mod for version pinning strategy
func CheckHealth(ctx context.Context, client RedisClient) (*HealthCheckInfo, error) {
	info := &HealthCheckInfo{}

	// Step 1: Basic connectivity (works for all)
	if err := client.Ping(ctx); err != nil {
		info.ErrorMessage = fmt.Sprintf("ping failed: %v", err)
		return info, err
	}
	info.BasicConnectivity = true

	// Step 2: Detect mode using library-level client type detection
	// This requires accessing the underlying redis.UniversalClient
	info.Mode = "standalone"

	// Try to extract underlying client from PrefixedClient (same package access)
	if prefixed, ok := client.(*PrefixedClient); ok {
		// Access the unexported client field (same package allows this)
		underlyingClient := prefixed.client

		// Type assert to *redis.Client to access Options()
		if redisClient, ok := underlyingClient.(*redis.Client); ok {
			// Get client options - FailoverClient sets Addr="FailoverClient"
			// See: vendor/github.com/redis/go-redis/v9/sentinel.go:122
			opts := redisClient.Options()
			if opts != nil && opts.Addr == "FailoverClient" {
				info.Mode = "sentinel"
			}
		}
	}

	// Step 3: Retrieve replication metrics using INFO (regardless of mode)
	// These are informational metrics, not health criteria
	infoStr, err := client.Info(ctx, "replication")
	if err != nil {
		// If INFO fails after successful ping, this is a health issue
		info.ErrorMessage = fmt.Sprintf("INFO replication failed: %v", err)
		return info, err
	}

	// Parse replication info for metrics
	lines := strings.Split(infoStr, "\r\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "connected_slaves:") {
			countStr := strings.TrimPrefix(line, "connected_slaves:")
			info.ConnectedSlaves, _ = strconv.Atoi(countStr)
		}
		if strings.HasPrefix(line, "master_repl_offset:") {
			offsetStr := strings.TrimPrefix(line, "master_repl_offset:")
			info.MasterReplOffset, _ = strconv.ParseInt(offsetStr, 10, 64)
		}
	}

	// Step 4: Check read-replica health if configured
	if prefixed, ok := client.(*PrefixedClient); ok && prefixed.replica != nil {
		info.ReplicaConfigured = true
		if replicaPingErr := prefixed.replica.Ping(ctx); replicaPingErr == nil {
			info.ReplicaHealthy = true
		}
		// Replica failure is informational only — reads fall back to master
	}

	// Health check passes - connectivity is good
	// Note: Slave count is informational only, not a health failure criterion
	// A Sentinel master with 0 slaves is still healthy from connectivity perspective
	info.IsHealthy = true
	return info, nil
}
