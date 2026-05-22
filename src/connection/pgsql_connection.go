// Package connection contains the PGSQLConnection type and methods for manipulating and querying a PostgreSQL connection
package connection

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/newrelic/infra-integrations-sdk/v3/log"
	"github.com/newrelic/nri-postgresql/src/args"
)

const (
	extensionsQuery = `
    SELECT -- EXTENSIONS_LIST
           n.nspname AS schema,
           e.extname AS extension
      FROM pg_extension AS e
      JOIN pg_namespace AS n ON n.oid = e.extnamespace;`
)

// PGSQLConnection represents a wrapper around a PostgreSQL connection.
// When CollectConnectionTiming is enabled, Timing is populated after the first Ping.
// When CollectQueryTelemetry is enabled, every Query/QueryUnsafe/Queryx call is timed
// and accumulated; drain with DrainTelemetry().
type PGSQLConnection struct {
	connection *sqlx.DB
	database   string
	Timing     *ConnectionTiming // non-nil when COLLECT_CONNECTION_TIMING=true
	// telemetry is a pointer so that value-receiver method calls (which receive a copy of the
	// struct) still accumulate into the same underlying storage as the original.
	telemetry *telemetryAccumulator
}

// Info holds all the information needed from the user to create a new connection
type Info interface {
	NewConnection(database string) (*PGSQLConnection, error)
	HostPort() (string, string)
	DatabaseName() string
}

type connectionInfo struct {
	Database                string
	Username                string
	Password                string
	Host                    string
	Port                    string
	Timeout                 string
	EnableSSL               bool
	SSLCertLocation         string
	SSLRootCertLocation     string
	SSLKeyLocation          string
	TrustServerCertificate  bool
	CollectConnectionTiming bool
	CollectQueryTelemetry   bool
}

// DefaultConnectionInfo takes an argument list and constructs a default connection out of it
func DefaultConnectionInfo(al *args.ArgumentList) Info {
	return &connectionInfo{
		Database:                al.Database,
		Username:                al.Username,
		Password:                al.Password,
		Host:                    al.Hostname,
		Port:                    al.Port,
		Timeout:                 al.Timeout,
		EnableSSL:               al.EnableSSL,
		SSLCertLocation:         al.SSLCertLocation,
		SSLRootCertLocation:     al.SSLRootCertLocation,
		SSLKeyLocation:          al.SSLKeyLocation,
		TrustServerCertificate:  al.TrustServerCertificate,
		CollectConnectionTiming: al.CollectConnectionTiming,
		CollectQueryTelemetry:   al.CollectQueryTelemetry,
	}
}

// NewConnection creates a new PGSQLConnection.
// Uses pgx/v5/stdlib as the driver (replaces lib/pq).
// When CollectConnectionTiming is enabled, a DialFunc is attached that measures
// DNS and TCP time on the first query's connection establishment — no extra Ping
// is issued, so there is no additional round-trip to the server.
func (ci *connectionInfo) NewConnection(database string) (*PGSQLConnection, error) {
	urlStr := createConnectionURL(ci, database)

	pgConn := &PGSQLConnection{
		database:  database,
		telemetry: &telemetryAccumulator{enabled: ci.CollectQueryTelemetry},
	}

	config, err := pgx.ParseConfig(urlStr)
	if err != nil {
		return nil, err
	}

	if ci.CollectConnectionTiming {
		timing := &ConnectionTiming{}
		pgConn.Timing = timing
		attachTimingDialFunc(config, timing)
	}

	// OpenDB holds a reference to the parsed config directly — no global registry needed.
	// Using RegisterConnConfig/UnregisterConnConfig would race: the deferred unregister fires
	// when NewConnection returns, but sqlx.DB is lazy and the driver looks up the config on
	// the first actual query, by which point the entry has already been removed.
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(1)
	pgConn.connection = sqlx.NewDb(db, "pgx")

	return pgConn, nil
}

func (ci *connectionInfo) HostPort() (string, string) {
	return ci.Host, ci.Port
}

func (ci *connectionInfo) DatabaseName() string {
	return ci.Database
}

// Ping verifies the connection to the database is still alive.
// When CollectConnectionTiming is enabled, calling Ping before the first query
// ensures the DialFunc fires and populates the Timing struct before it is read.
func (p PGSQLConnection) Ping() error {
	return p.connection.Ping()
}

// Close closes the PostgreSQL connection. If an error occurs it is logged as a warning.
func (p PGSQLConnection) Close() {
	if err := p.connection.Close(); err != nil {
		log.Warn("Unable to close PostgreSQL Connection: %s", err.Error())
	}
}

// Query runs a query and loads results into v.
// When CollectQueryTelemetry is enabled, execution time and any error are accumulated.
func (p PGSQLConnection) Query(v interface{}, query string) error {
	if p.telemetry != nil && p.telemetry.enabled {
		return p.timedSelect(v, query)
	}
	return p.connection.Select(v, query)
}

// QueryUnsafe runs a query and loads results into v, ignoring extra columns in the result set.
// This is useful for queries where the schema may vary (e.g., PgBouncer versions).
func (p PGSQLConnection) QueryUnsafe(v interface{}, query string) error {
	if p.telemetry != nil && p.telemetry.enabled {
		return p.timedSelectUnsafe(v, query)
	}
	return p.connection.Unsafe().Select(v, query)
}

// Queryx runs a query and returns a set of rows.
func (p PGSQLConnection) Queryx(query string) (*sqlx.Rows, error) {
	if p.telemetry != nil && p.telemetry.enabled {
		start := time.Now()
		rows, err := p.connection.Queryx(query)
		p.telemetry.record(extractQueryName(query), p.database, time.Since(start), err)
		return rows, err
	}
	return p.connection.Queryx(query)
}

// QueryxContext runs a query with the provided context and returns a set of rows.
// The context deadline is honoured by the driver, allowing callers to bound
// query execution time (e.g. the availability check timeout).
func (p PGSQLConnection) QueryxContext(ctx context.Context, query string) (*sqlx.Rows, error) {
	if p.telemetry != nil && p.telemetry.enabled {
		start := time.Now()
		rows, err := p.connection.QueryxContext(ctx, query)
		p.telemetry.record(extractQueryName(query), p.database, time.Since(start), err)
		return rows, err
	}
	return p.connection.QueryxContext(ctx, query)
}

type extensions map[string]map[string]bool

type extensionRow struct {
	SchemaName    string `db:"schema"`
	ExtensionName string `db:"extension"`
}

func (p PGSQLConnection) getExtensions() (extensions, error) {
	var extensionRows []*extensionRow
	if err := p.Query(&extensionRows, extensionsQuery); err != nil {
		log.Warn("Failure acquiring list of extensions: %+v", err)
		return nil, err
	}

	extensionList := make(extensions)
	for _, row := range extensionRows {
		if _, ok := extensionList[row.ExtensionName]; !ok {
			extensionList[row.ExtensionName] = make(map[string]bool)
		}

		if len(row.SchemaName) > 0 {
			extensionList[row.ExtensionName][row.SchemaName] = true
		}
	}

	return extensionList, nil
}

// HaveExtensionInSchema checks to see if the given Extension is
// installed on the current database in the given schema
func (p PGSQLConnection) HaveExtensionInSchema(extensionName, schemaName string) bool {
	extensions, err := p.getExtensions()
	if err != nil {
		return false
	}

	if _, ok := extensions[extensionName]; !ok {
		return false
	}

	if _, ok := extensions[extensionName][schemaName]; !ok {
		return false
	}

	return true
}

// createConnectionURL creates the connection string. A list of parameters
// can be found here https://godoc.org/github.com/lib/pq#hdr-Connection_String_Parameters
func createConnectionURL(ci *connectionInfo, database string) string {
	connectionURL := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(ci.Username, ci.Password),
		Host:   fmt.Sprintf("%s:%s", ci.Host, ci.Port),
		Path:   database,
	}

	query := url.Values{}
	query.Add("connect_timeout", ci.Timeout)

	// SSL settings
	if ci.EnableSSL {
		addSSLQueries(query, ci)
	} else {
		query.Add("sslmode", "disable")
	}

	connectionURL.RawQuery = query.Encode()
	return connectionURL.String()
}

// addSSLQueries add SSL query parameters
func addSSLQueries(query url.Values, ci *connectionInfo) {
	if ci.SSLCertLocation != "" {
		query.Add("sslcert", ci.SSLCertLocation)
	}
	if ci.SSLKeyLocation != "" {
		query.Add("sslkey", ci.SSLKeyLocation)
	}

	if ci.TrustServerCertificate {
		query.Add("sslmode", "require")
	} else {
		query.Add("sslmode", "verify-full")
		query.Add("sslrootcert", ci.SSLRootCertLocation)
	}
}
