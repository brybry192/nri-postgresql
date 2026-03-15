// Package availability provides explicit database availability checking for nri-postgresql.
// An implicit availability signal is always present — if NewConnection succeeds the DB is up.
// This package handles the optional explicit check: running a configurable canary query to
// confirm the server can process queries, not just accept connections.
package availability

import (
	"time"

	"github.com/newrelic/nri-postgresql/src/connection"
)

const DefaultQuery = "SELECT 1"

// CheckResult holds the outcome of an explicit availability check.
type CheckResult struct {
	Available    bool
	DurationMs   float64
	ErrorCode    string
	ErrorMessage string
	Query        string
}

// ExplicitCheck runs the given query against conn and returns a CheckResult.
// The query should be lightweight — the default is "SELECT 1".
// A non-error result with at least one row is considered available.
func ExplicitCheck(conn *connection.PGSQLConnection, query string) *CheckResult {
	result := &CheckResult{Query: query}

	start := time.Now()
	rows, err := conn.Queryx(query)
	result.DurationMs = float64(time.Since(start)) / float64(time.Millisecond)

	if err != nil {
		result.Available = false
		result.ErrorCode, result.ErrorMessage = connection.ClassifyError(err)
		return result
	}
	defer rows.Close()

	result.Available = rows.Next()
	if rowErr := rows.Err(); rowErr != nil {
		result.Available = false
		result.ErrorCode, result.ErrorMessage = connection.ClassifyError(rowErr)
	}

	return result
}
