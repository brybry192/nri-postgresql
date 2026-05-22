package connection

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// QueryTelemetry holds APM-style telemetry for a single query execution.
type QueryTelemetry struct {
	// QueryName is extracted from an embedded SQL comment (e.g. "-- BGWRITER_STATS")
	// or derived from the first keyword of the query if no comment is present.
	QueryName    string
	Database     string
	DurationMs   float64
	HasError     bool
	ErrorCode    string
	ErrorMessage string
}

// queryNameRe matches the first single-line SQL comment token: -- SOME_NAME
var queryNameRe = regexp.MustCompile(`--\s+([A-Z][A-Z0-9_]+)`)

// pgConnCredsRe matches the userinfo portion of a PostgreSQL connection URL:
// postgres://user:password@host:port/db → redacts "password" to "***".
// Uses a greedy quantifier so passwords containing @ are fully captured —
// the last @ before a hostname is the real delimiter.
var pgConnCredsRe = regexp.MustCompile(`://([^:]+):(.+)@([^@]+)`)

// sanitizeErrorMessage redacts credentials that may appear in error messages,
// particularly PostgreSQL connection URLs in the form postgres://user:password@host:port/db.
func sanitizeErrorMessage(msg string) string {
	return pgConnCredsRe.ReplaceAllString(msg, "://${1}:***@${3}")
}

// extractQueryName pulls a name from an embedded SQL comment like "-- BGWRITER_STATS".
// Falls back to the first SQL keyword + next token (e.g. "SHOW server_version").
func extractQueryName(query string) string {
	if m := queryNameRe.FindStringSubmatch(query); len(m) == 2 {
		return m[1]
	}
	// Fallback: first two space-separated words, upper-cased, no newlines
	words := strings.Fields(strings.ReplaceAll(query, "\n", " "))
	if len(words) == 0 {
		return "UNKNOWN"
	}
	if len(words) == 1 {
		return strings.ToUpper(words[0])
	}
	return strings.ToUpper(words[0] + "_" + words[1])
}

// ClassifiedError is an error that has already been classified by a prior call
// to ClassifyError. When this type is passed back into ClassifyError, the
// pre-classified code and message are returned directly without re-classification.
type ClassifiedError struct {
	Code string
	Msg  string
}

func (e *ClassifiedError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

// ClassifyError converts a raw error into a structured (code, message) pair.
// Exported so it can be used for connection-level errors in pgsql_connection.go.
func ClassifyError(err error) (code, message string) {
	if err == nil {
		return "", ""
	}

	// Already classified — pass through without re-classification.
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		return ce.Code, ce.Msg
	}

	message = sanitizeErrorMessage(err.Error())

	// Context deadline exceeded or cancellation — e.g. availability check timeout.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout", message
	}

	// PostgreSQL server error — structured SQLSTATE code
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		class := pgErr.Code
		if len(class) >= 2 {
			class = class[:2]
		}
		return fmt.Sprintf("pg_error_%s", class), pgErr.Message
	}

	// Network / I/O errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "timeout", message
		}
	}

	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "connection refused"):
		return "connection_refused", message
	case strings.Contains(lower, "eof"):
		return "server_closed_connection", message
	case strings.Contains(lower, "connection reset"):
		return "connection_reset", message
	case strings.Contains(lower, "no such host"),
		strings.Contains(lower, "dns"), strings.Contains(lower, "lookup"):
		return "dns_resolution_failed", message
	case strings.Contains(lower, "i/o timeout"), strings.Contains(lower, "io timeout"):
		return "io_timeout", message
	case strings.Contains(lower, "tls") || strings.Contains(lower, "certificate"):
		return "tls_error", message
	default:
		return "unknown_error", message
	}
}

// telemetryAccumulator is embedded in PGSQLConnection to collect per-query telemetry.
type telemetryAccumulator struct {
	enabled bool
	mu      sync.Mutex
	entries []*QueryTelemetry
}

func (a *telemetryAccumulator) record(queryName, database string, duration time.Duration, err error) {
	if !a.enabled {
		return
	}
	t := &QueryTelemetry{
		QueryName:  queryName,
		Database:   database,
		DurationMs: msec(duration),
	}
	if err != nil {
		t.HasError = true
		t.ErrorCode, t.ErrorMessage = ClassifyError(err)
	}
	a.mu.Lock()
	a.entries = append(a.entries, t)
	a.mu.Unlock()
}

// DrainTelemetry returns all accumulated query telemetry and resets the internal slice.
func (p *PGSQLConnection) DrainTelemetry() []*QueryTelemetry {
	p.telemetry.mu.Lock()
	defer p.telemetry.mu.Unlock()
	entries := p.telemetry.entries
	p.telemetry.entries = nil
	return entries
}

// timedSelect wraps connection.Select with wall-clock measurement and telemetry accumulation.
func (p *PGSQLConnection) timedSelect(v interface{}, query string) error {
	start := time.Now()
	err := p.connection.Select(v, query)
	p.telemetry.record(extractQueryName(query), p.database, time.Since(start), err)
	return err
}

// timedSelectUnsafe is the unsafe variant (ignores extra columns) with timing.
func (p *PGSQLConnection) timedSelectUnsafe(v interface{}, query string) error {
	start := time.Now()
	err := p.connection.Unsafe().Select(v, query)
	p.telemetry.record(extractQueryName(query), p.database, time.Since(start), err)
	return err
}
