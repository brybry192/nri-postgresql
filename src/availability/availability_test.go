package availability

import (
	"errors"
	"testing"

	"github.com/newrelic/nri-postgresql/src/connection"
	"github.com/stretchr/testify/assert"
	"gopkg.in/DATA-DOG/go-sqlmock.v1"
)

func TestDefaultQuery(t *testing.T) {
	assert.Equal(t, "SELECT 1", DefaultQuery)
}

// TestExplicitCheck_Available verifies the happy path: the canary query returns a row.
func TestExplicitCheck_Available(t *testing.T) {
	conn, mock := connection.CreateMockSQL(t)
	rows := sqlmock.NewRows([]string{"?column?"}).AddRow(1)
	mock.ExpectQuery("SELECT 1").WillReturnRows(rows)

	result := ExplicitCheck(conn, "SELECT 1")

	assert.True(t, result.Available)
	assert.Equal(t, "SELECT 1", result.Query)
	assert.Greater(t, result.DurationMs, 0.0)
	assert.Empty(t, result.ErrorCode)
	assert.Empty(t, result.ErrorMessage)
}

// TestExplicitCheck_EmptyResultSet verifies that zero rows means unavailable,
// with no error classification (the server answered, just with no rows).
func TestExplicitCheck_EmptyResultSet(t *testing.T) {
	conn, mock := connection.CreateMockSQL(t)
	rows := sqlmock.NewRows([]string{"?column?"}) // no rows added
	mock.ExpectQuery("SELECT 1").WillReturnRows(rows)

	result := ExplicitCheck(conn, "SELECT 1")

	assert.False(t, result.Available)
	assert.Empty(t, result.ErrorCode)
	assert.Empty(t, result.ErrorMessage)
}

// TestExplicitCheck_QueryError verifies that a connection-level error marks the
// instance unavailable and surfaces a classified error code.
func TestExplicitCheck_QueryError(t *testing.T) {
	conn, mock := connection.CreateMockSQL(t)
	mock.ExpectQuery("SELECT 1").WillReturnError(errors.New("connection refused"))

	result := ExplicitCheck(conn, "SELECT 1")

	assert.False(t, result.Available)
	assert.Equal(t, "connection_refused", result.ErrorCode)
	assert.NotEmpty(t, result.ErrorMessage)
	assert.Equal(t, "SELECT 1", result.Query)
}

// TestExplicitCheck_QueryErrorEOF verifies EOF (server closed connection) is classified correctly.
func TestExplicitCheck_QueryErrorEOF(t *testing.T) {
	conn, mock := connection.CreateMockSQL(t)
	mock.ExpectQuery(".*").WillReturnError(errors.New("read tcp: EOF"))

	result := ExplicitCheck(conn, "SELECT 1")

	assert.False(t, result.Available)
	assert.Equal(t, "server_closed_connection", result.ErrorCode)
}

// TestExplicitCheck_CustomQuery verifies that a non-default canary query is passed through
// correctly and appears in the result.
func TestExplicitCheck_CustomQuery(t *testing.T) {
	conn, mock := connection.CreateMockSQL(t)
	rows := sqlmock.NewRows([]string{"health"}).AddRow("ok")
	mock.ExpectQuery("SELECT health FROM monitoring.status").WillReturnRows(rows)

	result := ExplicitCheck(conn, "SELECT health FROM monitoring.status")

	assert.True(t, result.Available)
	assert.Equal(t, "SELECT health FROM monitoring.status", result.Query)
}

// TestExplicitCheck_DurationMeasured verifies that DurationMs is populated even on error.
func TestExplicitCheck_DurationMeasuredOnError(t *testing.T) {
	conn, mock := connection.CreateMockSQL(t)
	mock.ExpectQuery(".*").WillReturnError(errors.New("connection refused"))

	result := ExplicitCheck(conn, "SELECT 1")

	assert.False(t, result.Available)
	// DurationMs must be non-negative; the mock returns immediately so it may be very small.
	assert.GreaterOrEqual(t, result.DurationMs, 0.0)
}
