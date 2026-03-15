package connection

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"gopkg.in/DATA-DOG/go-sqlmock.v1"
)

// mockNetError implements net.Error so we can test the Timeout() branch in ClassifyError.
type mockNetError struct {
	msg     string
	timeout bool
}

func (e *mockNetError) Error() string   { return e.msg }
func (e *mockNetError) Timeout() bool   { return e.timeout }
func (e *mockNetError) Temporary() bool { return false }

// ---------------------------------------------------------------------------
// ClassifyError
// ---------------------------------------------------------------------------

func TestClassifyError_Nil(t *testing.T) {
	code, msg := ClassifyError(nil)
	assert.Equal(t, "", code)
	assert.Equal(t, "", msg)
}

func TestClassifyError_PgErrorAuth(t *testing.T) {
	err := &pgconn.PgError{Code: "28P01", Message: "password authentication failed"}
	code, msg := ClassifyError(err)
	assert.Equal(t, "pg_error_28", code)
	assert.Equal(t, "password authentication failed", msg)
}

func TestClassifyError_PgErrorSyntax(t *testing.T) {
	err := &pgconn.PgError{Code: "42601", Message: "syntax error at or near SELECT"}
	code, msg := ClassifyError(err)
	assert.Equal(t, "pg_error_42", code)
	assert.Equal(t, "syntax error at or near SELECT", msg)
}

func TestClassifyError_NetTimeout(t *testing.T) {
	err := &mockNetError{msg: "dial tcp: i/o timeout", timeout: true}
	code, msg := ClassifyError(err)
	assert.Equal(t, "timeout", code)
	assert.Equal(t, "dial tcp: i/o timeout", msg)
}

// A net.Error that is not a timeout falls through to string matching.
func TestClassifyError_NetErrorNonTimeout(t *testing.T) {
	err := &mockNetError{msg: "connection refused", timeout: false}
	code, _ := ClassifyError(err)
	assert.Equal(t, "connection_refused", code)
}

func TestClassifyError_ConnectionRefused(t *testing.T) {
	code, msg := ClassifyError(errors.New("dial tcp [::1]:5432: connect: connection refused"))
	assert.Equal(t, "connection_refused", code)
	assert.Contains(t, msg, "connection refused")
}

func TestClassifyError_EOF(t *testing.T) {
	code, _ := ClassifyError(errors.New("read tcp: EOF"))
	assert.Equal(t, "server_closed_connection", code)
}

func TestClassifyError_ConnectionReset(t *testing.T) {
	code, _ := ClassifyError(errors.New("read tcp: connection reset by peer"))
	assert.Equal(t, "connection_reset", code)
}

func TestClassifyError_NoSuchHost(t *testing.T) {
	code, _ := ClassifyError(errors.New("dial tcp: no such host: db.example.invalid"))
	assert.Equal(t, "dns_resolution_failed", code)
}

func TestClassifyError_DNSKeyword(t *testing.T) {
	code, _ := ClassifyError(errors.New("dns: lookup failed for host"))
	assert.Equal(t, "dns_resolution_failed", code)
}

func TestClassifyError_LookupKeyword(t *testing.T) {
	code, _ := ClassifyError(errors.New("lookup db.example.invalid: no such host"))
	assert.Equal(t, "dns_resolution_failed", code)
}

func TestClassifyError_IOTimeout(t *testing.T) {
	code, _ := ClassifyError(errors.New("read: i/o timeout"))
	assert.Equal(t, "io_timeout", code)
}

func TestClassifyError_IOTimeoutAlt(t *testing.T) {
	code, _ := ClassifyError(errors.New("io timeout waiting for response"))
	assert.Equal(t, "io_timeout", code)
}

func TestClassifyError_TLS(t *testing.T) {
	code, _ := ClassifyError(errors.New("tls: certificate signed by unknown authority"))
	assert.Equal(t, "tls_error", code)
}

func TestClassifyError_Certificate(t *testing.T) {
	code, _ := ClassifyError(errors.New("x509: certificate verify failed"))
	assert.Equal(t, "tls_error", code)
}

func TestClassifyError_Unknown(t *testing.T) {
	code, msg := ClassifyError(errors.New("something completely unexpected happened"))
	assert.Equal(t, "unknown_error", code)
	assert.Equal(t, "something completely unexpected happened", msg)
}

// ---------------------------------------------------------------------------
// extractQueryName
// ---------------------------------------------------------------------------

func TestExtractQueryName_EmbeddedComment(t *testing.T) {
	query := "SELECT -- BGWRITER_STATS\n scheduled_checkpoints FROM pg_stat_bgwriter"
	assert.Equal(t, "BGWRITER_STATS", extractQueryName(query))
}

func TestExtractQueryName_CommentAtStart(t *testing.T) {
	query := "-- EXTENSIONS_LIST\nSELECT * FROM pg_extension"
	assert.Equal(t, "EXTENSIONS_LIST", extractQueryName(query))
}

func TestExtractQueryName_NoComment_FallbackTwoWords(t *testing.T) {
	assert.Equal(t, "SELECT_*", extractQueryName("SELECT * FROM pg_stat_user_tables"))
}

func TestExtractQueryName_NoComment_SingleWord(t *testing.T) {
	assert.Equal(t, "SHOW", extractQueryName("SHOW"))
}

func TestExtractQueryName_EmptyQuery(t *testing.T) {
	assert.Equal(t, "UNKNOWN", extractQueryName(""))
}

func TestExtractQueryName_LowercaseCommentNotMatched(t *testing.T) {
	// Regex requires [A-Z] start; lowercase comment should not match.
	query := "SELECT 1 -- lowercase_comment"
	assert.Equal(t, "SELECT_1", extractQueryName(query))
}

func TestExtractQueryName_ShowQuery(t *testing.T) {
	// Mirrors the real version query used in the integration.
	assert.Equal(t, "SHOW_SERVER_VERSION", extractQueryName("SHOW server_version"))
}

// ---------------------------------------------------------------------------
// telemetryAccumulator
// ---------------------------------------------------------------------------

func TestTelemetryAccumulator_DisabledIsNoOp(t *testing.T) {
	acc := &telemetryAccumulator{enabled: false}
	acc.record("TEST_QUERY", "postgres", 5*time.Millisecond, nil)
	assert.Nil(t, acc.entries)
}

func TestTelemetryAccumulator_RecordSuccess(t *testing.T) {
	acc := &telemetryAccumulator{enabled: true}
	acc.record("TEST_QUERY", "postgres", 10*time.Millisecond, nil)
	assert.Len(t, acc.entries, 1)
	e := acc.entries[0]
	assert.Equal(t, "TEST_QUERY", e.QueryName)
	assert.Equal(t, "postgres", e.Database)
	assert.InDelta(t, 10.0, e.DurationMs, 0.1)
	assert.False(t, e.HasError)
	assert.Empty(t, e.ErrorCode)
	assert.Empty(t, e.ErrorMessage)
}

func TestTelemetryAccumulator_RecordWithError(t *testing.T) {
	acc := &telemetryAccumulator{enabled: true}
	acc.record("FAIL_QUERY", "mydb", 2*time.Millisecond, errors.New("connection refused"))
	assert.Len(t, acc.entries, 1)
	e := acc.entries[0]
	assert.True(t, e.HasError)
	assert.Equal(t, "connection_refused", e.ErrorCode)
	assert.NotEmpty(t, e.ErrorMessage)
}

func TestTelemetryAccumulator_MultipleRecords(t *testing.T) {
	acc := &telemetryAccumulator{enabled: true}
	acc.record("Q1", "db", time.Millisecond, nil)
	acc.record("Q2", "db", time.Millisecond, nil)
	acc.record("Q3", "db", time.Millisecond, nil)
	assert.Len(t, acc.entries, 3)
}

// ---------------------------------------------------------------------------
// DrainTelemetry
// ---------------------------------------------------------------------------

func TestDrainTelemetry_ReturnsAndResets(t *testing.T) {
	conn, _ := CreateMockSQL(t)
	conn.telemetry.enabled = true
	conn.telemetry.entries = []*QueryTelemetry{
		{QueryName: "Q1", DurationMs: 5.0},
		{QueryName: "Q2", DurationMs: 10.0},
	}
	first := conn.DrainTelemetry()
	assert.Len(t, first, 2)
	// Second drain must return nil — slice was reset, not just emptied.
	second := conn.DrainTelemetry()
	assert.Nil(t, second)
}

func TestDrainTelemetry_EmptyBeforeAnyRecords(t *testing.T) {
	conn, _ := CreateMockSQL(t)
	conn.telemetry.enabled = true
	assert.Nil(t, conn.DrainTelemetry())
}

// TestTelemetryAccumulator_ConcurrentRecord validates that concurrent calls to record
// do not race. Run with: go test -race ./src/connection/...
func TestTelemetryAccumulator_ConcurrentRecord(t *testing.T) {
	acc := &telemetryAccumulator{enabled: true}
	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			acc.record("CONCURRENT", "db", time.Millisecond, nil)
		}()
	}
	wg.Wait()
	assert.Len(t, acc.entries, goroutines)
}

// ---------------------------------------------------------------------------
// timedSelect / timedSelectUnsafe
// ---------------------------------------------------------------------------

func TestTimedSelect_AccumulatesTelemetry(t *testing.T) {
	conn, sqlMock := CreateMockSQL(t)
	conn.telemetry.enabled = true
	conn.database = "testdb"

	var result []struct {
		Val int `db:"val"`
	}
	rows := sqlmock.NewRows([]string{"val"}).AddRow(42)
	sqlMock.ExpectQuery(".*TEST_QUERY.*").WillReturnRows(rows)

	err := conn.timedSelect(&result, "SELECT -- TEST_QUERY\n val FROM t")
	assert.NoError(t, err)

	entries := conn.DrainTelemetry()
	assert.Len(t, entries, 1)
	assert.Equal(t, "TEST_QUERY", entries[0].QueryName)
	assert.Equal(t, "testdb", entries[0].Database)
	assert.False(t, entries[0].HasError)
	assert.Greater(t, entries[0].DurationMs, 0.0)
}

func TestTimedSelect_RecordsError(t *testing.T) {
	conn, sqlMock := CreateMockSQL(t)
	conn.telemetry.enabled = true
	conn.database = "testdb"

	var result []struct{ Val int `db:"val"` }
	sqlMock.ExpectQuery(".*").WillReturnError(errors.New("connection refused"))

	err := conn.timedSelect(&result, "SELECT val FROM t")
	assert.Error(t, err)

	entries := conn.DrainTelemetry()
	assert.Len(t, entries, 1)
	assert.True(t, entries[0].HasError)
	assert.Equal(t, "connection_refused", entries[0].ErrorCode)
}

func TestTimedSelectUnsafe_AccumulatesTelemetry(t *testing.T) {
	conn, sqlMock := CreateMockSQL(t)
	conn.telemetry.enabled = true
	conn.database = "pgbouncer"

	var result []struct {
		Database string `db:"database"`
	}
	rows := sqlmock.NewRows([]string{"database", "extra_col"}).AddRow("pgbouncer", "ignored")
	sqlMock.ExpectQuery(".*PGBOUNCER_QUERY.*").WillReturnRows(rows)

	err := conn.timedSelectUnsafe(&result, "SELECT -- PGBOUNCER_QUERY\n database FROM pgbouncer_stats")
	assert.NoError(t, err)

	entries := conn.DrainTelemetry()
	assert.Len(t, entries, 1)
	assert.Equal(t, "PGBOUNCER_QUERY", entries[0].QueryName)
	assert.Equal(t, "pgbouncer", entries[0].Database)
	assert.False(t, entries[0].HasError)
}

func TestTimedSelectUnsafe_RecordsError(t *testing.T) {
	conn, sqlMock := CreateMockSQL(t)
	conn.telemetry.enabled = true
	conn.database = "testdb"

	var result []struct{ Val int `db:"val"` }
	sqlMock.ExpectQuery(".*").WillReturnError(errors.New("EOF"))

	err := conn.timedSelectUnsafe(&result, "SELECT val FROM t")
	assert.Error(t, err)

	entries := conn.DrainTelemetry()
	assert.Len(t, entries, 1)
	assert.True(t, entries[0].HasError)
	assert.Equal(t, "server_closed_connection", entries[0].ErrorCode)
}
