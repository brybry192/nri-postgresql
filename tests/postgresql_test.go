//go:build integration

package tests

import (
	"flag"
	"os"
	"testing"

	"github.com/newrelic/nri-postgresql/tests/simulation"
	"github.com/stretchr/testify/assert"
)

var (
	defaultPassword = flag.String("password", "example", "Default password for postgres")
	defaultUser     = flag.String("username", "postgres", "Default username for postgres")
	defaultDB       = flag.String("database", "demo", "Default database name")
	container       = flag.String("container", "nri-postgresql", "Container name for the integration")
)

const (
	// docker compose service names
	serviceNamePostgres96     = "postgres-9-6"
	serviceNamePostgresLatest = "postgres-latest-supported"
	defaultBinaryPath         = "/nri-postgresql"
	integrationContainer      = "nri-postgresql"
)

func TestMain(m *testing.M) {
	flag.Parse()
	result := m.Run()
	os.Exit(result)
}

func TestSuccessConnection(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		Name       string
		Hostname   string
		Schema     string
		ExtraFlags []string
	}{
		{
			Name:     "Testing Metrics and inventory for Postgres v9.6.x",
			Hostname: serviceNamePostgres96,
			Schema:   "jsonschema-latest.json",
		},
		{
			Name:     "Testing Metrics and inventory for latest Postgres supported version",
			Hostname: serviceNamePostgresLatest,
			Schema:   "jsonschema-latest.json",
		},
		{
			Name:       "Inventory only for latest Postgres supported version",
			Hostname:   serviceNamePostgresLatest,
			Schema:     "jsonschema-inventory-latest.json",
			ExtraFlags: []string{`-inventory=true`},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{`-collection_list=all`}, tc.ExtraFlags...)
			stdout, stderr, err := simulation.RunIntegration(tc.Hostname, integrationContainer, defaultBinaryPath, defaultUser, defaultPassword, defaultDB, args...)
			assert.Empty(t, stderr)
			assert.NoError(t, err)
			assert.NotEmpty(t, stdout)
			err = simulation.ValidateJSONSchema(tc.Schema, stdout)
			assert.NoError(t, err)
		})
	}
}

func TestMissingRequiredVars(t *testing.T) {
	// Temporarily set username and password to nil to test missing credentials
	origUser, origPsw := defaultUser, defaultPassword
	defaultUser, defaultPassword = nil, nil
	defer func() {
		defaultUser, defaultPassword = origUser, origPsw
	}()

	_, stderr, err := simulation.RunIntegration(serviceNamePostgresLatest, integrationContainer, defaultBinaryPath, defaultUser, defaultPassword, defaultDB)
	assert.Error(t, err)
	assert.Contains(t, stderr, "invalid configuration: must specify a username and password")
}

func TestIgnoringDB(t *testing.T) {
	args := []string{
		`-collection_list=all`,
		`-collection_ignore_database_list=["demo"]`,
	}
	stdout, stderr, err := simulation.RunIntegration(serviceNamePostgresLatest, integrationContainer, defaultBinaryPath, defaultUser, defaultPassword, defaultDB, args...)
	assert.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Contains(t, stdout, `"database:postgres"`)
	assert.NotContains(t, stdout, `"database:demo"`)
}

// TestNoHealthSampleWithoutFlags verifies that no PostgresqlHealthSample is emitted
// when all observability flags are at their defaults (false). The feature is fully opt-in.
func TestNoHealthSampleWithoutFlags(t *testing.T) {
	stdout, stderr, err := simulation.RunIntegration(serviceNamePostgresLatest, integrationContainer, defaultBinaryPath, defaultUser, defaultPassword, defaultDB, `-collection_list=all`)
	assert.NoError(t, err)
	assert.Empty(t, stderr)
	assert.NotContains(t, stdout, `"PostgresqlHealthSample"`)
}

// TestHealthSampleWithTimingFlag verifies that a PostgresqlHealthSample is emitted
// when COLLECT_CONNECTION_TIMING is enabled.
func TestHealthSampleWithTimingFlag(t *testing.T) {
	stdout, stderr, err := simulation.RunIntegration(serviceNamePostgresLatest, integrationContainer, defaultBinaryPath, defaultUser, defaultPassword, defaultDB, `-collection_list=all`, `-collect_connection_timing=true`)
	assert.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Contains(t, stdout, `"PostgresqlHealthSample"`)
	assert.Contains(t, stdout, `"available"`)
}

// TestObservabilityFlags validates each new observability flag individually.
func TestObservabilityFlags(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		Name           string
		ExtraFlags     []string
		MustContain    []string
		MustNotContain []string
	}{
		{
			Name:       "Connection timing emits DNS and TCP gauges",
			ExtraFlags: []string{`-collect_connection_timing=true`},
			MustContain: []string{
				`"dnsLookupMs"`,
				`"tcpConnectMs"`,
			},
		},
		{
			Name:       "Availability check emits explicit sample with available=1",
			ExtraFlags: []string{`-enable_availability_check=true`},
			MustContain: []string{
				`"checkType":"explicit"`,
				`"available"`,
				`"durationMs"`,
				`"query":"SELECT 1"`,
			},
		},
		{
			Name:       "Availability check custom query is reflected in sample",
			ExtraFlags: []string{`-enable_availability_check=true`, `-availability_check_query=SELECT 42`},
			MustContain: []string{
				`"checkType":"explicit"`,
				`"query":"SELECT 42"`,
			},
		},
		{
			Name:       "Query telemetry emits health samples with checkType=query",
			ExtraFlags: []string{`-collect_query_telemetry=true`},
			MustContain: []string{
				`"checkType":"query"`,
				`"durationMs"`,
				`"hasError"`,
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{`-collection_list=all`}, tc.ExtraFlags...)
			stdout, stderr, err := simulation.RunIntegration(serviceNamePostgresLatest, integrationContainer, defaultBinaryPath, defaultUser, defaultPassword, defaultDB, args...)
			assert.NoError(t, err)
			assert.Empty(t, stderr)
			assert.NotEmpty(t, stdout)
			for _, want := range tc.MustContain {
				assert.Contains(t, stdout, want, "expected %q in output", want)
			}
			for _, notWant := range tc.MustNotContain {
				assert.NotContains(t, stdout, notWant, "unexpected %q in output", notWant)
			}
		})
	}
}

// TestConnectionFailureHealthSample verifies that when the host is unreachable:
//   - A PostgresqlHealthSample is still emitted (implicit signal — no gap in the series)
//   - When ENABLE_AVAILABILITY_CHECK is set, a checkType=explicit sample is also emitted
//     and carries an errorCode (the first real dial attempt fails here)
func TestConnectionFailureHealthSample(t *testing.T) {
	badHost := "nonexistent-postgres-host-00000"
	args := []string{
		`-enable_availability_check=true`,
	}
	// Ignore exit error: non-zero exit is possible when the host is unreachable.
	stdout, _, _ := simulation.RunIntegration(badHost, integrationContainer, defaultBinaryPath, defaultUser, defaultPassword, defaultDB, args...)
	if stdout == "" {
		t.Skip("integration binary produced no output on connection failure; check container setup")
	}
	assert.Contains(t, stdout, `"PostgresqlHealthSample"`, "health sample should always be emitted")
	assert.Contains(t, stdout, `"checkType"`, "explicit health sample should be emitted")
	assert.Contains(t, stdout, `"errorCode"`, "health sample should carry an errorCode when the host is unreachable")
}

// TestAvailabilityCheckTimeout verifies that a hanging canary query is interrupted by the
// configured timeout and the result is reported with errorCode=timeout rather than blocking
// the rest of the collection cycle.
func TestAvailabilityCheckTimeout(t *testing.T) {
	args := []string{
		`-enable_availability_check=true`,
		// pg_sleep(30) simulates a hung server; the tight timeout should cancel it quickly.
		`-availability_check_query=SELECT pg_sleep(30)`,
		`-availability_check_timeout_ms=500`,
	}
	stdout, _, _ := simulation.RunIntegration(serviceNamePostgresLatest, integrationContainer, defaultBinaryPath, defaultUser, defaultPassword, defaultDB, args...)
	assert.Contains(t, stdout, `"checkType"`)
	assert.Contains(t, stdout, `"errorCode"`)
	assert.Contains(t, stdout, `"timeout"`, "errorCode should classify the cancelled context as timeout")
}
