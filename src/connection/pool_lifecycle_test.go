package connection

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/DATA-DOG/go-sqlmock.v1"
)

func TestPoolClosedAfterSuccessfulUse(t *testing.T) {
	con, mock := CreateMockSQL(t)
	require.NotNil(t, con)

	// Simulate a normal query cycle
	mock.ExpectQuery("SELECT 1").WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))

	var result []struct {
		Val int `db:"?column?"`
	}
	err := con.Query(&result, "SELECT 1")
	assert.NoError(t, err)

	// Close should succeed and not panic
	con.Close()

	// Verify all expectations were met
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestPoolClosedAfterQueryError(t *testing.T) {
	con, mock := CreateMockSQL(t)

	mock.ExpectQuery("SELECT").WillReturnError(assert.AnError)

	var result []struct{ Val int }
	err := con.Query(&result, "SELECT bad")
	assert.Error(t, err)

	// Close should still work even after query errors
	con.Close()
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCloseIsIdempotent(t *testing.T) {
	con, _ := CreateMockSQL(t)

	// First close
	con.Close()

	// Second close should not panic (it logs a warning)
	con.Close()
}

func TestTelemetryAccumulatorInitializedOnNewConnection(t *testing.T) {
	con, _ := CreateMockSQL(t)
	// The mock creator doesn't set enabled=true, but the accumulator should exist
	require.NotNil(t, con.telemetry)
}

func TestTimingIsNilWhenNotEnabled(t *testing.T) {
	con, _ := CreateMockSQL(t)
	assert.Nil(t, con.Timing, "Timing should be nil when COLLECT_CONNECTION_TIMING is not enabled")
}
