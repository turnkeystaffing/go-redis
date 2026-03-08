package redis

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/redis/go-redis/v9"
)

// ClientConfig holds minimal configuration needed for Redis client creation
// This allows pkg/redis to remain independent of internal/auth packages
type ClientConfig struct {
	// Standalone mode
	Host     string
	Port     int
	Password string
	DB       int

	// Connection pool settings
	PoolSize     int
	MinIdleConns int
	MaxRetries   int

	// Sentinel mode (if enabled)
	SentinelEnabled    bool
	SentinelMasterName string
	SentinelAddresses  []string
	SentinelPassword   string

	// TLS settings
	TLSEnabled            bool
	TLSCertPath           string
	TLSKeyPath            string
	TLSCACertPath         string
	TLSInsecureSkipVerify bool
}

// NewUniversalClient creates a redis.UniversalClient that supports both standalone and Sentinel modes
// The UniversalClient interface automatically handles failover in Sentinel mode
func NewUniversalClient(cfg *ClientConfig) (redis.UniversalClient, error) {
	// Build TLS config if enabled
	var tlsConfig *tls.Config
	if cfg.TLSEnabled {
		var err error
		tlsConfig, err = buildTLSConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to build TLS config: %w", err)
		}
	}

	// Sentinel mode
	if cfg.SentinelEnabled {
		if cfg.SentinelMasterName == "" {
			return nil, fmt.Errorf("sentinel_master_name is required when sentinel_enabled=true")
		}
		if len(cfg.SentinelAddresses) == 0 {
			return nil, fmt.Errorf("sentinel_addresses is required when sentinel_enabled=true")
		}

		return redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       cfg.SentinelMasterName,
			SentinelAddrs:    cfg.SentinelAddresses,
			SentinelPassword: cfg.SentinelPassword,
			Password:         cfg.Password,
			DB:               cfg.DB,
			PoolSize:         cfg.PoolSize,
			MinIdleConns:     cfg.MinIdleConns,
			MaxRetries:       cfg.MaxRetries,
			TLSConfig:        tlsConfig,
			Protocol:         2,
			DisableIdentity:  true,
		}), nil
	}

	// Standalone mode
	return redis.NewClient(&redis.Options{
		Addr:            fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Password:        cfg.Password,
		DB:              cfg.DB,
		PoolSize:        cfg.PoolSize,
		MinIdleConns:    cfg.MinIdleConns,
		MaxRetries:      cfg.MaxRetries,
		TLSConfig:       tlsConfig,
		Protocol:        2,
		DisableIdentity: true,
	}), nil
}

// NewReadOnlyFailoverClient creates a Redis client that connects to Sentinel replicas only.
// Used for eventually-consistent reads (caches, admin queries, informational data).
// Requires Sentinel mode — returns error if Sentinel is not configured.
func NewReadOnlyFailoverClient(cfg *ClientConfig) (redis.UniversalClient, error) {
	if !cfg.SentinelEnabled {
		return nil, fmt.Errorf("read-only failover client requires sentinel_enabled=true")
	}
	if cfg.SentinelMasterName == "" {
		return nil, fmt.Errorf("sentinel_master_name is required when sentinel_enabled=true")
	}
	if len(cfg.SentinelAddresses) == 0 {
		return nil, fmt.Errorf("sentinel_addresses is required when sentinel_enabled=true")
	}

	var tlsConfig *tls.Config
	if cfg.TLSEnabled {
		var err error
		tlsConfig, err = buildTLSConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to build TLS config for replica: %w", err)
		}
	}

	return redis.NewFailoverClient(&redis.FailoverOptions{
		MasterName:       cfg.SentinelMasterName,
		SentinelAddrs:    cfg.SentinelAddresses,
		SentinelPassword: cfg.SentinelPassword,
		Password:         cfg.Password,
		DB:               cfg.DB,
		PoolSize:         cfg.PoolSize,
		MinIdleConns:     cfg.MinIdleConns,
		MaxRetries:       cfg.MaxRetries,
		TLSConfig:        tlsConfig,
		Protocol:         2,
		DisableIdentity:  true,
		ReplicaOnly:      true, // Route ALL operations to replicas
	}), nil
}

// buildTLSConfig creates a TLS configuration from certificate files
func buildTLSConfig(cfg *ClientConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: cfg.TLSInsecureSkipVerify,
	}

	// Load client certificate if provided
	if cfg.TLSCertPath != "" && cfg.TLSKeyPath != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertPath, cfg.TLSKeyPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load client cert/key: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	// Load CA certificate if provided
	if cfg.TLSCACertPath != "" {
		caCert, err := os.ReadFile(cfg.TLSCACertPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA cert: %w", err)
		}

		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA cert")
		}
		tlsConfig.RootCAs = caCertPool
	}

	return tlsConfig, nil
}
