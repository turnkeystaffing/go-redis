package redis

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultProbeInterval is the default interval between recovery probe pings.
	DefaultProbeInterval = 5 * time.Second

	// DefaultProbeTimeout is the default timeout for a single probe ping.
	DefaultProbeTimeout = 2 * time.Second
)

// DegradationMonitor provides Redis health state for fallback consumers.
// Consumers call IsDegraded() before attempting Redis operations, and
// MarkDegraded() when they encounter infrastructure errors.
type DegradationMonitor interface {
	// IsDegraded returns true when Redis is unavailable (consumers should use fallback).
	IsDegraded() bool

	// MarkDegraded signals that a Redis operation failed. Starts a background
	// recovery probe if not already running. Safe for concurrent use.
	MarkDegraded()
}

// HealthMonitorConfig configures the HealthMonitor probe behavior.
type HealthMonitorConfig struct {
	// ProbeInterval is how often the recovery probe pings Redis. Default: 5s.
	ProbeInterval time.Duration

	// ProbeTimeout is the maximum time for a single probe ping. Default: 2s.
	ProbeTimeout time.Duration
}

// HealthMonitor implements DegradationMonitor with a single background probe
// goroutine shared across all Redis consumers (rate limiter, auth cache, etc.).
//
// When any consumer calls MarkDegraded(), the monitor switches to degraded state
// and starts a probe that pings Redis at the configured interval. On successful
// ping, the monitor recovers and stops the probe.
type HealthMonitor struct {
	client        RedisClient
	logger        *slog.Logger
	probeInterval time.Duration
	probeTimeout  time.Duration

	degraded atomic.Bool

	probeMu   sync.Mutex
	probeStop chan struct{}
}

// NewHealthMonitor creates a shared Redis health monitor.
// Zero-value config fields use defaults (ProbeInterval=5s, ProbeTimeout=2s).
// Panics if client or logger is nil.
func NewHealthMonitor(client RedisClient, logger *slog.Logger, cfg HealthMonitorConfig) *HealthMonitor {
	if client == nil {
		panic("redis.NewHealthMonitor: client cannot be nil")
	}
	if logger == nil {
		panic("redis.NewHealthMonitor: logger cannot be nil")
	}

	interval := cfg.ProbeInterval
	if interval <= 0 {
		interval = DefaultProbeInterval
	}
	timeout := cfg.ProbeTimeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}

	return &HealthMonitor{
		client:        client,
		logger:        logger,
		probeInterval: interval,
		probeTimeout:  timeout,
	}
}

// Compile-time interface assertions.
var _ DegradationMonitor = (*HealthMonitor)(nil)

// IsDegraded returns true when Redis is unavailable.
func (m *HealthMonitor) IsDegraded() bool {
	return m.degraded.Load()
}

// MarkDegraded switches to degraded state and starts the recovery probe.
// Only logs and starts the probe on the first transition to degraded.
func (m *HealthMonitor) MarkDegraded() {
	if !m.degraded.Swap(true) {
		m.logger.Warn("Redis degraded — consumers falling back to in-memory")
		m.startProbe()
	}
}

// Close stops the probe goroutine if running.
func (m *HealthMonitor) Close() error {
	m.stopProbe()
	return nil
}

// startProbe launches a background goroutine that periodically pings Redis.
// Safe to call multiple times — only one probe runs at a time.
func (m *HealthMonitor) startProbe() {
	m.probeMu.Lock()
	defer m.probeMu.Unlock()

	if m.probeStop != nil {
		return
	}

	m.probeStop = make(chan struct{})
	go m.probeLoop(m.probeStop)
}

// stopProbe stops the running probe goroutine if any.
func (m *HealthMonitor) stopProbe() {
	m.probeMu.Lock()
	defer m.probeMu.Unlock()

	if m.probeStop != nil {
		close(m.probeStop)
		m.probeStop = nil
	}
}

// probeLoop periodically pings Redis until it recovers or stop is signaled.
func (m *HealthMonitor) probeLoop(stop chan struct{}) {
	ticker := time.NewTicker(m.probeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), m.probeTimeout)
			err := m.client.Ping(ctx)
			cancel()

			if err == nil {
				m.degraded.Store(false)
				m.logger.Info("Redis recovered — resuming normal operation")

				m.probeMu.Lock()
				m.probeStop = nil
				m.probeMu.Unlock()
				return
			}
		}
	}
}
