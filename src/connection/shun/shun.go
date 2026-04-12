// Package shun implements error-aware backoff for nri-postgresql.
//
// When the integration detects a persistent failure (auth_failed, ssl_error,
// dns_resolution_failed, connection_refused), it "shuns" the instance — skipping
// real connection attempts for an exponentially growing number of cycles while
// still emitting available=0 health samples so NRQL alerts keep firing.
//
// State is persisted to a JSON file so it survives the short-lived binary's restarts.
package shun

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/newrelic/infra-integrations-sdk/v3/log"
)

// Default backoff parameters (in cycles, not seconds).
const (
	InitialBackoffCycles = 2
	MaxBackoffCycles     = 60
)

// Clock abstracts time for deterministic testing.
type Clock interface {
	Now() time.Time
}

// RealClock uses the system clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// shunnableErrors are error codes that indicate a persistent problem unlikely to
// self-resolve without human intervention. These warrant exponential backoff.
var shunnableErrors = map[string]bool{
	"auth_failed":           true,
	"pg_error_28":           true, // auth class SQLSTATE
	"ssl_error":             true,
	"tls_error":             true,
	"dns_resolution_failed": true,
	"connection_refused":    true,
}

// IsShunnableError returns true if the error code represents a persistent failure
// that warrants exponential backoff.
func IsShunnableError(errorCode string) bool {
	if shunnableErrors[errorCode] {
		return true
	}
	// PG SQLSTATE class 28 (Invalid Authorization Specification) is always shunnable.
	if strings.HasPrefix(errorCode, "pg_error_28") {
		return true
	}
	return false
}

// InstanceState holds the backoff state for a single monitored instance.
type InstanceState struct {
	ShunUntilUnix                int64  `json:"shunUntilUnix"`
	ConsecutiveShunnableFailures int    `json:"consecutiveShunnableFailures"`
	LastErrorCode                string `json:"lastErrorCode"`
	LastErrorMessage             string `json:"lastErrorMessage,omitempty"`
	LastAttemptUnix              int64  `json:"lastAttemptUnix"`
	BackoffCycles                int    `json:"backoffCycles"`
}

// StateFile is the top-level JSON structure persisted to disk.
type StateFile struct {
	Version   int                       `json:"version"`
	Instances map[string]*InstanceState `json:"instances"`
}

// Manager manages shun state for all monitored instances.
type Manager struct {
	clock    Clock
	filePath string
	interval time.Duration // collection interval — used to convert cycles to wall-clock time
	state    *StateFile
}

// NewManager creates a Manager. If filePath is empty, state is in-memory only.
// The interval should match the NR infra agent's collection interval (typically 15–30s).
func NewManager(filePath string, interval time.Duration, clock Clock) *Manager {
	if clock == nil {
		clock = RealClock{}
	}
	return &Manager{
		clock:    clock,
		filePath: filePath,
		interval: interval,
		state: &StateFile{
			Version:   1,
			Instances: make(map[string]*InstanceState),
		},
	}
}

// LoadState reads shun state from the configured file. Missing or corrupt files
// are treated as empty state (first run or manual reset).
func (m *Manager) LoadState() {
	if m.filePath == "" {
		return
	}

	data, err := os.ReadFile(m.filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("Failed to read shun state file %s: %s", m.filePath, err)
		}
		return
	}

	var sf StateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		log.Warn("Corrupt shun state file %s (treating as empty): %s", m.filePath, err)
		return
	}
	if sf.Instances == nil {
		sf.Instances = make(map[string]*InstanceState)
	}
	m.state = &sf
}

// SaveState writes the current shun state to disk. Errors are logged but not fatal —
// the integration continues with in-memory state.
func (m *Manager) SaveState() {
	if m.filePath == "" {
		return
	}

	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		log.Warn("Failed to marshal shun state: %s", err)
		return
	}

	if err := os.WriteFile(m.filePath, data, 0644); err != nil {
		log.Warn("Failed to write shun state file %s: %s", m.filePath, err)
	}
}

// IsShunned returns true if the instance should skip connection attempts this cycle.
func (m *Manager) IsShunned(instanceKey string) bool {
	is, ok := m.state.Instances[instanceKey]
	if !ok {
		return false
	}
	return m.clock.Now().Unix() < is.ShunUntilUnix
}

// GetState returns the current shun state for an instance, or nil if not shunned.
func (m *Manager) GetState(instanceKey string) *InstanceState {
	return m.state.Instances[instanceKey]
}

// RecordFailure records a failed connection attempt. If the error is shunnable,
// exponential backoff is applied. Non-shunnable errors are recorded but don't
// trigger shunning.
func (m *Manager) RecordFailure(instanceKey, errorCode, errorMessage string) {
	now := m.clock.Now()

	is := m.state.Instances[instanceKey]
	if is == nil {
		is = &InstanceState{}
		m.state.Instances[instanceKey] = is
	}

	is.LastErrorCode = errorCode
	is.LastErrorMessage = errorMessage
	is.LastAttemptUnix = now.Unix()

	if !IsShunnableError(errorCode) {
		// Non-shunnable: record the attempt but don't backoff. Reset consecutive
		// count so a subsequent shunnable error starts fresh.
		is.ConsecutiveShunnableFailures = 0
		is.BackoffCycles = 0
		is.ShunUntilUnix = 0
		return
	}

	is.ConsecutiveShunnableFailures++

	// Exponential backoff: 2, 4, 8, 16, 32, 60 (capped)
	if is.BackoffCycles == 0 {
		is.BackoffCycles = InitialBackoffCycles
	} else {
		is.BackoffCycles *= 2
	}
	if is.BackoffCycles > MaxBackoffCycles {
		is.BackoffCycles = MaxBackoffCycles
	}

	shunDuration := time.Duration(is.BackoffCycles) * m.interval
	is.ShunUntilUnix = now.Add(shunDuration).Unix()
}

// RecordSuccess clears all shun state for the instance.
func (m *Manager) RecordSuccess(instanceKey string) {
	delete(m.state.Instances, instanceKey)
}

// ShunRemainingCycles returns the approximate number of collection cycles remaining
// in the shun period. Returns 0 if not shunned.
func (m *Manager) ShunRemainingCycles(instanceKey string) int {
	is := m.state.Instances[instanceKey]
	if is == nil {
		return 0
	}
	remaining := time.Unix(is.ShunUntilUnix, 0).Sub(m.clock.Now())
	if remaining <= 0 {
		return 0
	}
	cycles := int(remaining / m.interval)
	if remaining%m.interval > 0 {
		cycles++
	}
	return cycles
}
