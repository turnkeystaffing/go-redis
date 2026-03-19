package redis

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testMonitorLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, nil))
}

// --- mockPingClient ---

type mockPingClient struct {
	MockRedisClient
	pingErr atomic.Value // stores error or nil
}

func newMockPingClient(err error) *mockPingClient {
	c := &mockPingClient{}
	c.data = make(map[string]string)
	c.hashData = make(map[string]map[string]string)
	c.listData = make(map[string][]string)
	c.prefix = "test:"
	if err != nil {
		c.pingErr.Store(err)
	}
	return c
}

func (m *mockPingClient) Ping(_ context.Context) error {
	v := m.pingErr.Load()
	if v == nil {
		return nil
	}
	if err, ok := v.(error); ok {
		return err
	}
	return nil
}

func (m *mockPingClient) setPingErr(err error) {
	if err == nil {
		m.pingErr.Store((*error)(nil))
	} else {
		m.pingErr.Store(err)
	}
}

func (m *mockPingClient) clearPingErr() {
	m.pingErr = atomic.Value{}
}

// --- Constructor tests ---

func TestNewHealthMonitor_PanicsOnNilClient(t *testing.T) {
	t.Parallel()
	assert.PanicsWithValue(t, "redis.NewHealthMonitor: client cannot be nil", func() {
		NewHealthMonitor(nil, testMonitorLogger(), HealthMonitorConfig{})
	})
}

func TestNewHealthMonitor_PanicsOnNilLogger(t *testing.T) {
	t.Parallel()
	assert.PanicsWithValue(t, "redis.NewHealthMonitor: logger cannot be nil", func() {
		NewHealthMonitor(NewMockRedisClient(), nil, HealthMonitorConfig{})
	})
}

func TestNewHealthMonitor_StartsHealthy(t *testing.T) {
	t.Parallel()
	m := NewHealthMonitor(NewMockRedisClient(), testMonitorLogger(), HealthMonitorConfig{})
	defer m.Close()
	assert.False(t, m.IsDegraded())
}

func TestNewHealthMonitor_DefaultsApplied(t *testing.T) {
	t.Parallel()
	m := NewHealthMonitor(NewMockRedisClient(), testMonitorLogger(), HealthMonitorConfig{})
	defer m.Close()
	assert.Equal(t, DefaultProbeInterval, m.probeInterval)
	assert.Equal(t, DefaultProbeTimeout, m.probeTimeout)
}

func TestNewHealthMonitor_CustomConfig(t *testing.T) {
	t.Parallel()
	m := NewHealthMonitor(NewMockRedisClient(), testMonitorLogger(), HealthMonitorConfig{
		ProbeInterval: 10 * time.Second,
		ProbeTimeout:  3 * time.Second,
	})
	defer m.Close()
	assert.Equal(t, 10*time.Second, m.probeInterval)
	assert.Equal(t, 3*time.Second, m.probeTimeout)
}

// --- Degradation tests ---

func TestHealthMonitor_MarkDegraded(t *testing.T) {
	m := NewHealthMonitor(newMockPingClient(errors.New("down")), testMonitorLogger(), HealthMonitorConfig{})
	defer m.Close()

	assert.False(t, m.IsDegraded())
	m.MarkDegraded()
	assert.True(t, m.IsDegraded())
}

func TestHealthMonitor_MarkDegraded_Idempotent(t *testing.T) {
	m := NewHealthMonitor(newMockPingClient(errors.New("down")), testMonitorLogger(), HealthMonitorConfig{})
	defer m.Close()

	m.MarkDegraded()
	m.MarkDegraded()
	m.MarkDegraded()
	assert.True(t, m.IsDegraded())
}

func TestHealthMonitor_Recovery(t *testing.T) {
	client := newMockPingClient(errors.New("down"))
	m := NewHealthMonitor(client, testMonitorLogger(), HealthMonitorConfig{
		ProbeInterval: 100 * time.Millisecond,
		ProbeTimeout:  50 * time.Millisecond,
	})
	defer m.Close()

	m.MarkDegraded()
	require.True(t, m.IsDegraded())

	// Simulate Redis recovery
	client.clearPingErr()

	assert.Eventually(t, func() bool {
		return !m.IsDegraded()
	}, 2*time.Second, 50*time.Millisecond, "monitor should recover when Redis becomes available")
}

func TestHealthMonitor_Close_StopsProbe(t *testing.T) {
	client := newMockPingClient(errors.New("down"))
	m := NewHealthMonitor(client, testMonitorLogger(), HealthMonitorConfig{})

	m.MarkDegraded()
	require.True(t, m.IsDegraded())

	// Close should stop the probe without panic
	err := m.Close()
	assert.NoError(t, err)

	// Should still report degraded (Close doesn't reset state)
	assert.True(t, m.IsDegraded())
}

func TestHealthMonitor_Close_Idempotent(t *testing.T) {
	m := NewHealthMonitor(NewMockRedisClient(), testMonitorLogger(), HealthMonitorConfig{})

	assert.NoError(t, m.Close())
	assert.NoError(t, m.Close())
	assert.NoError(t, m.Close())
}

// --- Interface compliance ---

func TestHealthMonitor_ImplementsDegradationMonitor(t *testing.T) {
	t.Parallel()
	var _ DegradationMonitor = (*HealthMonitor)(nil)
}

// --- Concurrent access ---

func TestHealthMonitor_ConcurrentAccess(t *testing.T) {
	client := newMockPingClient(errors.New("down"))
	m := NewHealthMonitor(client, testMonitorLogger(), HealthMonitorConfig{})
	defer m.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			m.MarkDegraded()
			m.IsDegraded()
		}
	}()

	for range 100 {
		m.IsDegraded()
		m.MarkDegraded()
	}

	<-done
}
