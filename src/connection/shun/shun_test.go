package shun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock provides deterministic time for unit tests.
type fakeClock struct {
	current time.Time
}

func (c *fakeClock) Now() time.Time           { return c.current }
func (c *fakeClock) Advance(d time.Duration)  { c.current = c.current.Add(d) }
func (c *fakeClock) Set(t time.Time)          { c.current = t }

func newTestManager(clock *fakeClock) *Manager {
	return NewManager("", 15*time.Second, clock)
}

// --- Error classification ---

func TestShunnableErrorClassification(t *testing.T) {
	tests := []struct {
		errorCode string
		shunnable bool
	}{
		{"auth_failed", true},
		{"pg_error_28", true},
		{"pg_error_28P01", true}, // specific auth SQLSTATE
		{"ssl_error", true},
		{"tls_error", true},
		{"dns_resolution_failed", true},
		{"connection_refused", true},
		{"timeout", false},
		{"pg_error_57", false},
		{"pg_error_57014", false},
		{"unknown_error", false},
		{"server_closed_connection", false},
		{"connection_reset", false},
		{"io_timeout", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.errorCode, func(t *testing.T) {
			assert.Equal(t, tt.shunnable, IsShunnableError(tt.errorCode))
		})
	}
}

// --- Backoff state machine ---

func TestFirstShunnableFailureSetsBackoffTo2(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := newTestManager(clock)

	m.RecordFailure("pg-01:5432", "auth_failed", "password authentication failed")

	assert.True(t, m.IsShunned("pg-01:5432"))
	is := m.GetState("pg-01:5432")
	require.NotNil(t, is)
	assert.Equal(t, InitialBackoffCycles, is.BackoffCycles)
	assert.Equal(t, 1, is.ConsecutiveShunnableFailures)
	assert.Equal(t, "auth_failed", is.LastErrorCode)
}

func TestConsecutiveShunnableFailuresDoubleBackoff(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := newTestManager(clock)

	m.RecordFailure("pg-01:5432", "auth_failed", "password authentication failed")
	assert.Equal(t, 2, m.GetState("pg-01:5432").BackoffCycles)

	// Advance past the shun window so the next cycle would attempt a retry
	clock.Advance(time.Duration(2) * 15 * time.Second)
	m.RecordFailure("pg-01:5432", "auth_failed", "password authentication failed")
	assert.Equal(t, 4, m.GetState("pg-01:5432").BackoffCycles)
	assert.Equal(t, 2, m.GetState("pg-01:5432").ConsecutiveShunnableFailures)

	clock.Advance(time.Duration(4) * 15 * time.Second)
	m.RecordFailure("pg-01:5432", "auth_failed", "password authentication failed")
	assert.Equal(t, 8, m.GetState("pg-01:5432").BackoffCycles)
	assert.Equal(t, 3, m.GetState("pg-01:5432").ConsecutiveShunnableFailures)
}

func TestBackoffCapsAtMaximum(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := newTestManager(clock)

	// Drive backoff past the maximum: 2, 4, 8, 16, 32, 60, 60, 60...
	for i := 0; i < 10; i++ {
		clock.Advance(time.Duration(MaxBackoffCycles+1) * 15 * time.Second)
		m.RecordFailure("pg-01:5432", "auth_failed", "msg")
	}

	is := m.GetState("pg-01:5432")
	assert.Equal(t, MaxBackoffCycles, is.BackoffCycles)
	assert.Equal(t, 10, is.ConsecutiveShunnableFailures)
}

func TestSuccessClearsShunState(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := newTestManager(clock)

	// Shun with high backoff
	for i := 0; i < 4; i++ {
		clock.Advance(time.Duration(MaxBackoffCycles+1) * 15 * time.Second)
		m.RecordFailure("pg-01:5432", "auth_failed", "msg")
	}
	assert.True(t, m.IsShunned("pg-01:5432"))

	m.RecordSuccess("pg-01:5432")

	assert.False(t, m.IsShunned("pg-01:5432"))
	assert.Nil(t, m.GetState("pg-01:5432"))
}

func TestShunExpiresAfterTTL(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := newTestManager(clock)

	m.RecordFailure("pg-01:5432", "auth_failed", "msg")
	assert.True(t, m.IsShunned("pg-01:5432"))

	// Advance by exactly 2 cycles (30 seconds at 15s interval)
	clock.Advance(2 * 15 * time.Second)
	assert.False(t, m.IsShunned("pg-01:5432"))
}

func TestNonShunnableErrorsDoNotTriggerShunning(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := newTestManager(clock)

	m.RecordFailure("pg-01:5432", "timeout", "context deadline exceeded")
	assert.False(t, m.IsShunned("pg-01:5432"))

	// State is recorded but backoff is zero
	is := m.GetState("pg-01:5432")
	require.NotNil(t, is)
	assert.Equal(t, 0, is.BackoffCycles)
	assert.Equal(t, 0, is.ConsecutiveShunnableFailures)
	assert.Equal(t, "timeout", is.LastErrorCode)
}

func TestNonShunnableAfterShunnableResetsConsecutiveCount(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := newTestManager(clock)

	m.RecordFailure("pg-01:5432", "auth_failed", "msg")
	assert.Equal(t, 1, m.GetState("pg-01:5432").ConsecutiveShunnableFailures)

	// A non-shunnable error clears the consecutive count
	clock.Advance(time.Duration(3) * 15 * time.Second)
	m.RecordFailure("pg-01:5432", "timeout", "context deadline exceeded")
	assert.Equal(t, 0, m.GetState("pg-01:5432").ConsecutiveShunnableFailures)
	assert.Equal(t, 0, m.GetState("pg-01:5432").BackoffCycles)
	assert.False(t, m.IsShunned("pg-01:5432"))
}

func TestShunRemainingCycles(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := newTestManager(clock)

	m.RecordFailure("pg-01:5432", "connection_refused", "msg")
	assert.Equal(t, 2, m.ShunRemainingCycles("pg-01:5432"))

	clock.Advance(15 * time.Second)
	assert.Equal(t, 1, m.ShunRemainingCycles("pg-01:5432"))

	clock.Advance(15 * time.Second)
	assert.Equal(t, 0, m.ShunRemainingCycles("pg-01:5432"))

	// Unknown instance
	assert.Equal(t, 0, m.ShunRemainingCycles("nonexistent:5432"))
}

// --- Multi-instance independence ---

func TestMultipleInstancesAreIndependent(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := newTestManager(clock)

	// pg-01: shunned (auth_failed, backoff=8 → not expired yet)
	m.RecordFailure("pg-01:5432", "auth_failed", "msg")
	m.RecordFailure("pg-01:5432", "auth_failed", "msg")
	m.RecordFailure("pg-01:5432", "auth_failed", "msg")
	// backoff is now 8 cycles

	// pg-02: never failed
	// pg-03: shunned with backoff=2, then expired
	m.RecordFailure("pg-03:5432", "dns_resolution_failed", "no such host")
	clock.Advance(2 * 15 * time.Second) // pg-03's shun expires

	assert.True(t, m.IsShunned("pg-01:5432"), "pg-01 should still be shunned")
	assert.False(t, m.IsShunned("pg-02:5432"), "pg-02 was never shunned")
	assert.False(t, m.IsShunned("pg-03:5432"), "pg-03's shun should have expired")
}

// --- State file persistence ---

func TestStateFilePersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shun.json")
	clock := &fakeClock{current: time.Date(2026, 4, 12, 10, 0, 0, 0, time.UTC)}

	m1 := NewManager(path, 15*time.Second, clock)
	m1.RecordFailure("pg-01:5432", "auth_failed", "password authentication failed")
	m1.RecordFailure("pg-02:5432", "connection_refused", "connection refused")
	m1.SaveState()

	// New manager reads the file back
	m2 := NewManager(path, 15*time.Second, clock)
	m2.LoadState()

	assert.True(t, m2.IsShunned("pg-01:5432"))
	assert.True(t, m2.IsShunned("pg-02:5432"))
	assert.Equal(t, "auth_failed", m2.GetState("pg-01:5432").LastErrorCode)
	assert.Equal(t, "connection_refused", m2.GetState("pg-02:5432").LastErrorCode)
	assert.Equal(t, 1, m2.GetState("pg-01:5432").ConsecutiveShunnableFailures)
}

func TestStateFileCorruptTreatedAsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shun.json")
	require.NoError(t, os.WriteFile(path, []byte("{invalid json!!!"), 0644))

	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := NewManager(path, 15*time.Second, clock)
	m.LoadState()

	// Should be empty — no panic
	assert.False(t, m.IsShunned("pg-01:5432"))
	assert.Empty(t, m.state.Instances)
}

func TestStateFileMissingTreatedAsEmpty(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := NewManager("/nonexistent/path/shun.json", 15*time.Second, clock)
	m.LoadState()

	assert.False(t, m.IsShunned("pg-01:5432"))
	assert.Empty(t, m.state.Instances)
}

func TestStateFilePermissionDeniedOnWrite(t *testing.T) {
	dir := t.TempDir()
	roDir := filepath.Join(dir, "readonly")
	require.NoError(t, os.Mkdir(roDir, 0444))
	t.Cleanup(func() { os.Chmod(roDir, 0755) }) //nolint: errcheck
	path := filepath.Join(roDir, "shun.json")

	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := NewManager(path, 15*time.Second, clock)
	m.RecordFailure("pg-01:5432", "auth_failed", "msg")

	// Should not panic — just logs a warning
	m.SaveState()

	// In-memory state still works
	assert.True(t, m.IsShunned("pg-01:5432"))
}

func TestStateFileEmptyPath(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := NewManager("", 15*time.Second, clock)
	m.RecordFailure("pg-01:5432", "auth_failed", "msg")

	// Load/Save are no-ops — no panic
	m.LoadState()
	m.SaveState()

	assert.True(t, m.IsShunned("pg-01:5432"))
}

// --- Backoff progression table ---

func TestBackoffProgression(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := newTestManager(clock)

	expected := []int{2, 4, 8, 16, 32, 60, 60, 60}
	for i, want := range expected {
		// Advance past current shun
		clock.Advance(time.Duration(MaxBackoffCycles+1) * 15 * time.Second)
		m.RecordFailure("pg-01:5432", "ssl_error", "certificate expired")
		got := m.GetState("pg-01:5432").BackoffCycles
		assert.Equal(t, want, got, "iteration %d: backoff should be %d, got %d", i, want, got)
	}
}

// --- Edge cases ---

func TestNewManagerNilClockDefaultsToReal(t *testing.T) {
	m := NewManager("", 15*time.Second, nil)
	// Should not panic — uses RealClock
	assert.False(t, m.IsShunned("anything"))
}

func TestRecordSuccessOnUnknownInstanceIsNoOp(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := newTestManager(clock)
	// Should not panic
	m.RecordSuccess("nonexistent:5432")
	assert.Nil(t, m.GetState("nonexistent:5432"))
}

func TestStateFileVersionIsPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shun.json")
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}

	m := NewManager(path, 15*time.Second, clock)
	m.RecordFailure("pg-01:5432", "auth_failed", "msg")
	m.SaveState()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var sf StateFile
	require.NoError(t, json.Unmarshal(data, &sf))
	assert.Equal(t, 1, sf.Version)
}

func TestShunUntilIsWallClockBased(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 12, 0, 0, 0, time.UTC)}
	m := NewManager("", 15*time.Second, clock)

	m.RecordFailure("pg-01:5432", "auth_failed", "msg")
	is := m.GetState("pg-01:5432")

	// ShunUntil should be now + (2 cycles * 15 seconds) = now + 30s
	expectedUnix := clock.Now().Add(30 * time.Second).Unix()
	assert.Equal(t, expectedUnix, is.ShunUntilUnix)
}

func TestDifferentShunnableErrorsStillAccumulate(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)}
	m := newTestManager(clock)

	m.RecordFailure("pg-01:5432", "auth_failed", "msg1")
	assert.Equal(t, 2, m.GetState("pg-01:5432").BackoffCycles)

	clock.Advance(time.Duration(3) * 15 * time.Second)
	m.RecordFailure("pg-01:5432", "ssl_error", "msg2") // different shunnable error
	assert.Equal(t, 4, m.GetState("pg-01:5432").BackoffCycles)
	assert.Equal(t, 2, m.GetState("pg-01:5432").ConsecutiveShunnableFailures)
	assert.Equal(t, "ssl_error", m.GetState("pg-01:5432").LastErrorCode)
}
