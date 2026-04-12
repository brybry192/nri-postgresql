package metrics

import (
	"context"
	"fmt"
	"io/ioutil"
	"reflect"
	"regexp"
	"sync"
	"time"

	"github.com/blang/semver/v4"
	"github.com/newrelic/infra-integrations-sdk/v3/data/attribute"
	"github.com/newrelic/infra-integrations-sdk/v3/data/metric"
	"github.com/newrelic/infra-integrations-sdk/v3/integration"
	"github.com/newrelic/infra-integrations-sdk/v3/log"
	"github.com/newrelic/nri-postgresql/src/availability"
	"github.com/newrelic/nri-postgresql/src/collection"
	"github.com/newrelic/nri-postgresql/src/connection"
	"github.com/newrelic/nri-postgresql/src/connection/shun"
	yaml "gopkg.in/yaml.v3"
)

const (
	versionQuery = `SHOW server_version`

	healthSampleEventType = "PostgresqlHealthSample"
)

// ObservabilityConfig holds the optional APM-style observability settings passed to PopulateMetrics.
type ObservabilityConfig struct {
	CollectConnectionTiming    bool
	EnableAvailabilityCheck    bool
	AvailabilityCheckQuery     string
	AvailabilityCheckTimeoutMs int
	CollectQueryTelemetry      bool
}

// PopulateMetrics collects metrics for each type
func PopulateMetrics(
	ci connection.Info,
	databaseList collection.DatabaseList,
	instance *integration.Entity,
	i *integration.Integration,
	collectPgBouncer, collectDbLocks, collectBloat bool,
	customMetricsQuery string,
	obs ObservabilityConfig,
	shunMgr *shun.Manager) {

	instanceKey := instance.Metadata.Name // host:port

	// When shunned, skip the real connection attempt entirely. Emit cached health
	// samples so NRQL alerts continue to see available=0 during the backoff.
	if shunMgr != nil && shunMgr.IsShunned(instanceKey) {
		state := shunMgr.GetState(instanceKey)
		remaining := shunMgr.ShunRemainingCycles(instanceKey)
		log.Warn("Instance %s is shunned (%s, %d cycles remaining) — skipping connection",
			instanceKey, state.LastErrorCode, remaining)

		if obs.EnableAvailabilityCheck || obs.CollectConnectionTiming {
			publishShunnedHealthSample(instance, state, remaining)
		}
		return
	}

	con, err := ci.NewConnection(ci.DatabaseName())

	if err != nil {
		log.Error("Metrics collection failed: error creating connection to PostgreSQL: %s", err.Error())
		errCode, errMsg := connection.ClassifyError(err)
		if shunMgr != nil {
			shunMgr.RecordFailure(instanceKey, errCode, errMsg)
		}
		// Emit health samples on connection failure only when observability is enabled,
		// so there is no gap in the time series for alerting.
		if obs.EnableAvailabilityCheck || obs.CollectConnectionTiming {
			publishImplicitHealthSample(instance, con, err)
		}
		if obs.EnableAvailabilityCheck {
			q := obs.AvailabilityCheckQuery
			if q == "" {
				q = availability.DefaultQuery
			}
			publishExplicitHealthSample(instance, &availability.CheckResult{
				Available:    false,
				Query:        q,
				ErrorCode:    errCode,
				ErrorMessage: errMsg,
			})
		}
		return
	}
	defer con.Close()

	// Before publishing the implicit health sample, ensure the pgx DialFunc has fired
	// so that con.Timing carries real DNS/TCP values instead of zeros.
	//
	// When ENABLE_AVAILABILITY_CHECK is true the ExplicitCheck below triggers the dial.
	// When only COLLECT_CONNECTION_TIMING is true we issue a lightweight Ping instead.
	// Either way the order is: trigger dial → publishImplicitHealthSample → everything else.
	var explicitResult *availability.CheckResult
	var connErr error
	if obs.EnableAvailabilityCheck {
		q := obs.AvailabilityCheckQuery
		if q == "" {
			q = availability.DefaultQuery
		}
		timeoutMs := obs.AvailabilityCheckTimeoutMs
		if timeoutMs <= 0 {
			timeoutMs = 10000
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
		explicitResult = availability.ExplicitCheck(ctx, con, q)
		cancel()
		if !explicitResult.Available {
			connErr = &connection.ClassifiedError{Code: explicitResult.ErrorCode, Msg: explicitResult.ErrorMessage}
			if shunMgr != nil {
				shunMgr.RecordFailure(instanceKey, explicitResult.ErrorCode, explicitResult.ErrorMessage)
			}
		} else if shunMgr != nil {
			shunMgr.RecordSuccess(instanceKey)
		}
	} else if obs.CollectConnectionTiming {
		// No availability check — ping to trigger the DialFunc before reading con.Timing.
		connErr = con.Ping()
		if shunMgr != nil {
			if connErr != nil {
				errCode, errMsg := connection.ClassifyError(connErr)
				shunMgr.RecordFailure(instanceKey, errCode, errMsg)
			} else {
				shunMgr.RecordSuccess(instanceKey)
			}
		}
	} else if shunMgr != nil {
		// No observability flags, but shun manager active — connection succeeded.
		shunMgr.RecordSuccess(instanceKey)
	}

	// Emit health samples only when at least one observability flag is active.
	// With no flags set, the integration produces identical output to the upstream baseline.
	if obs.EnableAvailabilityCheck || obs.CollectConnectionTiming {
		publishImplicitHealthSample(instance, con, connErr)
	}

	if explicitResult != nil {
		publishExplicitHealthSample(instance, explicitResult)
	}

	version, err := CollectVersion(con)
	if err != nil {
		log.Error("Metrics collection failed: error collecting version number: %s", err.Error())
		return
	}

	PopulateInstanceMetrics(instance, version, con)
	PopulateDatabaseMetrics(databaseList, version, i, con, ci)
	if collectDbLocks {
		PopulateDatabaseLockMetrics(databaseList, version, i, con, ci)
	}
	PopulateTableMetrics(databaseList, version, i, ci, collectBloat)
	PopulateIndexMetrics(databaseList, i, ci)
	if customMetricsQuery != "" {
		PopulateCustomMetrics(customMetricsQuery, i, con, ci, instance)
	}

	// Drain and publish per-query health samples accumulated during the collection above.
	if obs.CollectQueryTelemetry {
		publishQueryHealthSamples(instance, con.DrainTelemetry())
	}

	if collectPgBouncer {
		pgbCon, pgbErr := ci.NewConnection("pgbouncer")
		if pgbErr != nil {
			log.Error("Error creating connection to pgbouncer database: %s", pgbErr)
		} else {
			defer pgbCon.Close()
			PopulatePgBouncerMetrics(i, pgbCon, ci)
			if obs.CollectQueryTelemetry {
				publishQueryHealthSamples(instance, pgbCon.DrainTelemetry())
			}
		}
	}
}

// healthSampleAttrs returns the standard attributes for a PostgresqlHealthSample.
func healthSampleAttrs(instance *integration.Entity) []attribute.Attribute {
	return []attribute.Attribute{
		{Key: "displayName", Value: instance.Metadata.Name},
		{Key: "entityName", Value: instance.Metadata.Namespace + ":" + instance.Metadata.Name},
	}
}

// publishImplicitHealthSample emits a PostgresqlHealthSample with checkType=implicit
// capturing the ping-based availability signal and connection phase timings.
func publishImplicitHealthSample(instance *integration.Entity, con *connection.PGSQLConnection, connErr error) {
	attrs := append(healthSampleAttrs(instance), attribute.Attribute{Key: "checkType", Value: "implicit"})
	ms := instance.NewMetricSet(healthSampleEventType, attrs...)

	hasError := 0.0
	if connErr != nil {
		hasError = 1.0
		setGauge(ms, "available", 0)
		errCode, errMsg := connection.ClassifyError(connErr)
		setAttribute(ms, "errorCode", errCode)
		setAttribute(ms, "errorMessage", errMsg)
	} else {
		setGauge(ms, "available", 1)
	}
	setGauge(ms, "hasError", hasError)

	if con != nil && con.Timing != nil {
		setGauge(ms, "dnsLookupMs", con.Timing.DNSLookupMs)
		setGauge(ms, "tcpConnectMs", con.Timing.TCPConnectMs)
		if con.Timing.TLSHandshakeMs > 0 {
			setGauge(ms, "tlsHandshakeMs", con.Timing.TLSHandshakeMs)
		}
	}
}

// publishExplicitHealthSample emits a PostgresqlHealthSample with checkType=explicit
// carrying the canary query result.
func publishExplicitHealthSample(instance *integration.Entity, result *availability.CheckResult) {
	attrs := append(healthSampleAttrs(instance), attribute.Attribute{Key: "checkType", Value: "explicit"})
	ms := instance.NewMetricSet(healthSampleEventType, attrs...)

	available := 0.0
	hasError := 0.0
	if result.Available {
		available = 1.0
	} else {
		hasError = 1.0
	}
	setGauge(ms, "available", available)
	setGauge(ms, "hasError", hasError)
	setGauge(ms, "durationMs", result.DurationMs)
	setAttribute(ms, "query", result.Query)
	if result.ErrorCode != "" {
		setAttribute(ms, "errorCode", result.ErrorCode)
		setAttribute(ms, "errorMessage", result.ErrorMessage)
	}
}

// publishShunnedHealthSample emits health samples for a shunned instance. No real
// connection is attempted — cached error info from the last failure is used. This
// ensures NRQL alert conditions continue to see the outage during backoff.
func publishShunnedHealthSample(instance *integration.Entity, state *shun.InstanceState, remainingCycles int) {
	// Implicit sample (checkType=implicit) with shunned=true
	implicitAttrs := append(healthSampleAttrs(instance), attribute.Attribute{Key: "checkType", Value: "implicit"})
	ims := instance.NewMetricSet(healthSampleEventType, implicitAttrs...)
	setGauge(ims, "available", 0)
	setGauge(ims, "hasError", 1)
	setAttribute(ims, "errorCode", state.LastErrorCode)
	setAttribute(ims, "errorMessage", state.LastErrorMessage)
	setAttribute(ims, "shunned", "true")
	setGauge(ims, "shunRemainingCycles", float64(remainingCycles))
	setGauge(ims, "shunBackoffCycles", float64(state.BackoffCycles))

	// Explicit sample (checkType=explicit) mirroring the same failure
	explicitAttrs := append(healthSampleAttrs(instance), attribute.Attribute{Key: "checkType", Value: "explicit"})
	ems := instance.NewMetricSet(healthSampleEventType, explicitAttrs...)
	setGauge(ems, "available", 0)
	setGauge(ems, "hasError", 1)
	setAttribute(ems, "errorCode", state.LastErrorCode)
	setAttribute(ems, "errorMessage", state.LastErrorMessage)
	setAttribute(ems, "shunned", "true")
	setGauge(ems, "shunRemainingCycles", float64(remainingCycles))
	setGauge(ems, "shunBackoffCycles", float64(state.BackoffCycles))
}

// publishQueryHealthSamples emits one PostgresqlHealthSample per internal monitoring
// query with checkType=query.
func publishQueryHealthSamples(instance *integration.Entity, entries []*connection.QueryTelemetry) {
	baseAttrs := healthSampleAttrs(instance)
	for _, t := range entries {
		entryAttrs := append(baseAttrs,
			attribute.Attribute{Key: "checkType", Value: "query"},
			attribute.Attribute{Key: "queryName", Value: t.QueryName},
			attribute.Attribute{Key: "database", Value: t.Database},
		)
		ms := instance.NewMetricSet(healthSampleEventType, entryAttrs...)
		setGauge(ms, "durationMs", t.DurationMs)
		hasError := 0.0
		if t.HasError {
			hasError = 1.0
		}
		setGauge(ms, "hasError", hasError)
		if t.ErrorCode != "" {
			setAttribute(ms, "errorCode", t.ErrorCode)
			setAttribute(ms, "errorMessage", t.ErrorMessage)
		}
	}
}

func setGauge(ms *metric.Set, name string, val float64) {
	if err := ms.SetMetric(name, val, metric.GAUGE); err != nil {
		log.Warn("Failed to set metric %s: %s", name, err)
	}
}

func setAttribute(ms *metric.Set, name, val string) {
	if err := ms.SetMetric(name, val, metric.ATTRIBUTE); err != nil {
		log.Warn("Failed to set attribute %s: %s", name, err)
	}
}

// PopulateCustomMetricsFromFile collects metrics defined by a custom config file
func PopulateCustomMetricsFromFile(ci connection.Info, configFile string, psqlIntegration *integration.Integration) {
	contents, err := ioutil.ReadFile(configFile)
	if err != nil {
		log.Error("Failed to read custom config file: %s", err)
		return
	}

	var customYAML customMetricsYAML
	err = yaml.Unmarshal(contents, &customYAML)
	if err != nil {
		log.Error("Failed to unmarshal custom config file: %s", err)
		return
	}

	// Semaphore to run 10 custom queries concurrently
	sem := make(chan struct{}, 10)
	wg := sync.WaitGroup{}
	for _, config := range customYAML.Queries {
		sem <- struct{}{}
		wg.Add(1)
		go func(cfg customMetricsConfig) {
			defer wg.Done()
			defer func() {
				<-sem
			}()

			CollectCustomConfig(ci, cfg, psqlIntegration)
		}(config)
	}
	wg.Wait()
}

// CollectCustomConfig collects metrics defined by a custom config
func CollectCustomConfig(ci connection.Info, cfg customMetricsConfig, pgIntegration *integration.Integration) {
	dbName := func() string {
		if cfg.Database == "" {
			return ci.DatabaseName()
		}
		return cfg.Database
	}()

	con, err := ci.NewConnection(dbName)
	if err != nil {
		log.Error("Custom query collection failed: error creating connection to PostgreSQL: %s", err.Error())
		return
	}
	defer con.Close()

	rows, err := con.Queryx(cfg.Query)
	if err != nil {
		log.Error("Could not execute database query: %s", err.Error())
		return
	}
	defer func() {
		_ = rows.Close()
	}()

	host, port := ci.HostPort()
	hostIDAttribute := integration.NewIDAttribute("host", host)
	portIDAttribute := integration.NewIDAttribute("port", port)
	databaseEntity, err := pgIntegration.Entity(dbName, "pg-database", hostIDAttribute, portIDAttribute)
	if err != nil {
		log.Error("Failed to create custom database entity: %s", err)
	}

	sampleName := func() string {
		if cfg.SampleName == "" {
			return "PostgresqlCustomSample"
		}
		return cfg.SampleName
	}()

	for rows.Next() {
		row := make(map[string]interface{})
		err := rows.MapScan(row)
		if err != nil {
			log.Error("Failed to scan custom query row: %s", err)
			return
		}

		ms := databaseEntity.NewMetricSet(sampleName, attribute.Attribute{
			Key: "database", Value: dbName,
		})

		for k, v := range row {
			sanitized := sanitizeValue(v)
			metricType := func() metric.SourceType {
				t, ok := cfg.MetricTypes[k]
				if !ok {
					return inferMetricType(sanitized)
				}
				return metric.SourceType(t)
			}()

			err := ms.SetMetric(k, sanitized, metricType)
			if err != nil {
				log.Warn("Failed to set metric: %s", err)
			}
		}
	}
}

func sanitizeValue(val interface{}) interface{} {
	switch v := val.(type) {
	case string, float32, float64, int, int32, int64:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func inferMetricType(val interface{}) metric.SourceType {
	switch val.(type) {
	case string:
		return metric.ATTRIBUTE
	case float32, float64, int, int32, int64:
		return metric.GAUGE
	default:
		return metric.ATTRIBUTE
	}
}

type metricType metric.SourceType

func (m *metricType) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var raw string
	err := unmarshal(&raw)
	if err != nil {
		return err
	}

	st, err := metric.SourceTypeForName(raw)
	if err != nil {
		return err
	}

	*m = metricType(st)
	return nil
}

type customMetricsYAML struct {
	Queries []customMetricsConfig
}

type customMetricsConfig struct {
	Query       string                `yaml:"query"`
	Database    string                `yaml:"database"`
	MetricTypes map[string]metricType `yaml:"metric_types"`
	SampleName  string                `yaml:"sample_name"`
}

type serverVersionRow struct {
	Version string `db:"server_version"`
}

func CollectVersion(connection *connection.PGSQLConnection) (*semver.Version, error) {
	var versionRows []*serverVersionRow
	if err := connection.Query(&versionRows, versionQuery); err != nil {
		return nil, err
	}

	re := regexp.MustCompile(`[0-9]+\.[0-9]+(\.[0-9])?`)
	version := re.FindString(versionRows[0].Version)
	// special cases for ubuntu/debian parsing
	//version := versionRows[0].Version
	//if strings.Contains(version, "Ubuntu") {
	//return parseSpecialVersion(version, strings.Index(version, " (Ubuntu"))
	//} else if strings.Contains(version, "Debian") {
	//return parseSpecialVersion(version, strings.Index(version, " (Debian"))
	//}

	v, err := semver.ParseTolerant(version)
	if err != nil {
		return nil, err
	}

	return &v, nil
}

// func parseSpecialVersion(version string, specialIndex int) (*semver.Version, error) {
// partialVersion := version[:specialIndex]

//v, err := semver.ParseTolerant(partialVersion)
//if err != nil {
//return nil, err
//}

//return &v, nil
//}

// PopulateInstanceMetrics populates the metrics for an instance
func PopulateInstanceMetrics(instanceEntity *integration.Entity, version *semver.Version, connection *connection.PGSQLConnection) {
	metricSet := instanceEntity.NewMetricSet("PostgresqlInstanceSample",
		attribute.Attribute{Key: "displayName", Value: instanceEntity.Metadata.Name},
		attribute.Attribute{Key: "entityName", Value: instanceEntity.Metadata.Namespace + ":" + instanceEntity.Metadata.Name},
	)

	for _, queryDef := range generateInstanceDefinitions(version) {
		dataModels := queryDef.GetDataModels()
		if err := connection.Query(dataModels, queryDef.GetQuery()); err != nil {
			log.Error("Could not execute instance query: %s", err.Error())
			continue
		}

		vp := reflect.Indirect(reflect.ValueOf(dataModels))

		// Nothing was returned
		if vp.Len() == 0 {
			log.Debug("No data returned from instance query '%s'", queryDef.GetQuery())
			continue
		}

		vpInterface := vp.Index(0).Interface()
		err := metricSet.MarshalMetrics(vpInterface)
		if err != nil {
			log.Error("Could not parse metrics from instance query result: %s", err.Error())
		}
	}
}

// PopulateDatabaseMetrics populates the metrics for a database
func PopulateDatabaseMetrics(databases collection.DatabaseList, version *semver.Version, pgIntegration *integration.Integration, connection *connection.PGSQLConnection, ci connection.Info) {
	databaseDefinitions := generateDatabaseDefinitions(databases, version)
	processDatabaseDefinitions(databaseDefinitions, pgIntegration, connection, ci)
}

// PopulateDatabaseLockMetrics populates the lock metrics for a database
func PopulateDatabaseLockMetrics(databases collection.DatabaseList, version *semver.Version, pgIntegration *integration.Integration, connection *connection.PGSQLConnection, ci connection.Info) {
	if !connection.HaveExtensionInSchema("tablefunc", "public") {
		log.Warn("Crosstab function not available; database lock metric gathering not possible.")
		log.Warn("To enable database lock metrics, enable the 'tablefunc' extension on the public")
		log.Warn("schema of your database. You can do so by:")
		log.Warn("  1. Installing the postgresql contribs package for your OS; and")
		log.Warn("  2. Run the query 'CREATE EXTENSION tablefunc;' against your database's public schema")
		return
	}

	lockDefinitions := generateLockDefinitions(databases)

	processDatabaseDefinitions(lockDefinitions, pgIntegration, connection, ci)
}

func processDatabaseDefinitions(definitions []*QueryDefinition, pgIntegration *integration.Integration, connection *connection.PGSQLConnection, ci connection.Info) {
	for _, queryDef := range definitions {
		// collect into model
		dataModels := queryDef.GetDataModels()
		if err := connection.Query(dataModels, queryDef.GetQuery()); err != nil {
			log.Error("Could not execute database query: %s", err.Error())
			continue
		}

		// for each row in the response
		v := reflect.Indirect(reflect.ValueOf(dataModels))
		for i := 0; i < v.Len(); i++ {
			db := v.Index(i).Interface()
			name, err := GetDatabaseName(db)
			if err != nil {
				log.Error("Unable to get database name: %s", err.Error())
			}

			host, port := ci.HostPort()
			hostIDAttribute := integration.NewIDAttribute("host", host)
			portIDAttribute := integration.NewIDAttribute("port", port)
			databaseEntity, err := pgIntegration.Entity(name, "pg-database", hostIDAttribute, portIDAttribute)
			if err != nil {
				log.Error("Failed to get database entity for name %s: %s", name, err.Error())
			}
			metricSet := databaseEntity.NewMetricSet("PostgresqlDatabaseSample",
				attribute.Attribute{Key: "displayName", Value: databaseEntity.Metadata.Name},
				attribute.Attribute{Key: "entityName", Value: "database:" + databaseEntity.Metadata.Name},
			)

			if err := metricSet.MarshalMetrics(db); err != nil {
				log.Error("Failed to database entity with metrics: %s", err.Error())
			}

		}
	}
}

// PopulateTableMetrics populates the metrics for a table
func PopulateTableMetrics(databases collection.DatabaseList, version *semver.Version, pgIntegration *integration.Integration, ci connection.Info, collectBloat bool) {
	for database, schemaList := range databases {
		if len(schemaList) == 0 {
			return
		}

		func() {
			con, err := ci.NewConnection(database)
			if err != nil {
				log.Error("Failed to connect to database %s: %s", database, err.Error())
				return
			}
			defer con.Close()
			populateTableMetricsForDatabase(schemaList, version, con, pgIntegration, ci, collectBloat)
		}()
	}
}

func populateTableMetricsForDatabase(schemaList collection.SchemaList, version *semver.Version, con *connection.PGSQLConnection, pgIntegration *integration.Integration, ci connection.Info, collectBloat bool) {
	tableDefinitions := generateTableDefinitions(schemaList, version, collectBloat)

	// collect into model
	for _, definition := range tableDefinitions {

		dataModels := definition.GetDataModels()
		if err := con.Query(dataModels, definition.GetQuery()); err != nil {
			log.Error("Could not execute table query: %s", err.Error())
			return
		}

		// for each row in the response
		v := reflect.Indirect(reflect.ValueOf(dataModels))
		for i := 0; i < v.Len(); i++ {
			row := v.Index(i).Interface()
			dbName, err := GetDatabaseName(row)
			if err != nil {
				log.Error("Unable to get database name: %s", err.Error())
			}
			schemaName, err := GetSchemaName(row)
			if err != nil {
				log.Error("Unable to get schema name: %s", err.Error())
			}
			tableName, err := GetTableName(row)
			if err != nil {
				log.Error("Unable to get table name: %s", err.Error())
			}

			host, port := ci.HostPort()
			hostIDAttribute := integration.NewIDAttribute("host", host)
			portIDAttribute := integration.NewIDAttribute("port", port)
			databaseIDAttribute := integration.NewIDAttribute("pg-database", dbName)
			schemaIDAttribute := integration.NewIDAttribute("pg-schema", schemaName)
			tableEntity, err := pgIntegration.Entity(tableName, "pg-table", hostIDAttribute, portIDAttribute, databaseIDAttribute, schemaIDAttribute)
			if err != nil {
				log.Error("Failed to get table entity for table %s: %s", tableName, err.Error())
			}
			metricSet := tableEntity.NewMetricSet("PostgresqlTableSample",
				attribute.Attribute{Key: "displayName", Value: tableEntity.Metadata.Name},
				attribute.Attribute{Key: "entityName", Value: "table:" + tableEntity.Metadata.Name},
				attribute.Attribute{Key: "database", Value: dbName},
				attribute.Attribute{Key: "schema", Value: schemaName},
			)

			if err := metricSet.MarshalMetrics(row); err != nil {
				log.Error("Failed to populate table entity with metrics: %s", err.Error())
			}

		}
	}
}

// PopulateIndexMetrics populates the metrics for an index
func PopulateIndexMetrics(databases collection.DatabaseList, pgIntegration *integration.Integration, ci connection.Info) {
	for database, schemaList := range databases {
		func() {
			con, err := ci.NewConnection(database)
			if err != nil {
				log.Error("Failed to create new connection to database %s: %s", database, err.Error())
				return
			}
			defer con.Close()
			populateIndexMetricsForDatabase(schemaList, con, pgIntegration, ci)
		}()
	}
}

func populateIndexMetricsForDatabase(schemaList collection.SchemaList, con *connection.PGSQLConnection, pgIntegration *integration.Integration, ci connection.Info) {
	indexDefinitions := generateIndexDefinitions(schemaList)

	for _, definition := range indexDefinitions {

		// collect into model
		dataModels := definition.GetDataModels()
		if err := con.Query(dataModels, definition.GetQuery()); err != nil {
			log.Error("Could not execute index query: %s", err.Error())
			return
		}

		// for each row in the response
		v := reflect.Indirect(reflect.ValueOf(dataModels))
		for i := 0; i < v.Len(); i++ {
			row := v.Index(i).Interface()
			dbName, err := GetDatabaseName(row)
			if err != nil {
				log.Error("Unable to get database name: %s", err.Error())
			}
			schemaName, err := GetSchemaName(row)
			if err != nil {
				log.Error("Unable to get schema name: %s", err.Error())
			}
			tableName, err := GetTableName(row)
			if err != nil {
				log.Error("Unable to get table name: %s", err.Error())
			}
			indexName, err := GetIndexName(row)
			if err != nil {
				log.Error("Unable to get index name: %s", err.Error())
			}

			host, port := ci.HostPort()
			hostIDAttribute := integration.NewIDAttribute("host", host)
			portIDAttribute := integration.NewIDAttribute("port", port)
			databaseIDAttribute := integration.NewIDAttribute("pg-database", dbName)
			schemaIDAttribute := integration.NewIDAttribute("pg-schema", schemaName)
			tableIDAttribute := integration.NewIDAttribute("pg-table", tableName)
			indexEntity, err := pgIntegration.Entity(indexName, "pg-index", hostIDAttribute, portIDAttribute, databaseIDAttribute, schemaIDAttribute, tableIDAttribute)
			if err != nil {
				log.Error("Failed to get table entity for index %s: %s", indexName, err.Error())
			}
			metricSet := indexEntity.NewMetricSet("PostgresqlIndexSample",
				attribute.Attribute{Key: "displayName", Value: indexEntity.Metadata.Name},
				attribute.Attribute{Key: "entityName", Value: "index:" + indexEntity.Metadata.Name},
				attribute.Attribute{Key: "database", Value: dbName},
				attribute.Attribute{Key: "schema", Value: schemaName},
				attribute.Attribute{Key: "table", Value: tableName},
			)

			if err := metricSet.MarshalMetrics(row); err != nil {
				log.Error("Failed to populate index entity with metrics: %s", err.Error())
			}

		}

	}
}

// PopulatePgBouncerMetrics populates pgbouncer metrics
func PopulatePgBouncerMetrics(pgIntegration *integration.Integration, con *connection.PGSQLConnection, ci connection.Info) {
	pgbouncerDefs := generatePgBouncerDefinitions()

	for _, definition := range pgbouncerDefs {
		dataModels := definition.GetDataModels()
		// Use QueryUnsafe to support different PgBouncer versions with varying column sets
		if err := con.QueryUnsafe(dataModels, definition.GetQuery()); err != nil {
			log.Error("Could not execute index query: %s", err.Error())
			return
		}

		// for each row in the response
		v := reflect.Indirect(reflect.ValueOf(dataModels))
		for i := 0; i < v.Len(); i++ {
			db := v.Index(i).Interface()
			name, err := GetDatabaseName(db)
			if err != nil {
				log.Error("Unable to get database name: %s", err.Error())
				continue
			}

			host, port := ci.HostPort()
			hostIDAttribute := integration.NewIDAttribute("host", host)
			portIDAttribute := integration.NewIDAttribute("port", port)
			pgEntity, err := pgIntegration.Entity(name, "pgbouncer", hostIDAttribute, portIDAttribute)
			if err != nil {
				log.Error("Failed to get database entity for name %s: %s", name, err.Error())
			}
			metricSet := pgEntity.NewMetricSet("PgBouncerSample",
				attribute.Attribute{Key: "displayName", Value: name},
				attribute.Attribute{Key: "entityName", Value: "pgbouncer:" + name},
				attribute.Attribute{Key: "host", Value: host},
			)

			if err := metricSet.MarshalMetrics(db); err != nil {
				log.Error("Failed to populate pgbouncer entity with metrics: %s", err.Error())
			}
		}
	}
}

// PopulateCustomMetrics collects metrics from a custom query
func PopulateCustomMetrics(customMetricsQuery string, pgIntegration *integration.Integration, con *connection.PGSQLConnection, ci connection.Info, instance *integration.Entity) {
	rows, err := con.Queryx(customMetricsQuery)
	if err != nil {
		log.Error("Could not execute database query: %s", err.Error())
		return
	}
	defer func() {
		_ = rows.Close()
	}()

	for rows.Next() {
		row := make(map[string]interface{})
		err := rows.MapScan(row)
		if err != nil {
			log.Error("Failed to scan custom query row: %s", err)
			return
		}

		nameInterface, ok := row["metric_name"]
		if !ok {
			log.Error("Missing required column 'metric_name' in custom query")
			return
		}
		name, ok := nameInterface.(string)
		if !ok {
			log.Error("Non-string type %T for custom query 'metric_name' column", nameInterface)
			continue
		}

		metricTypeInterface, ok := row["metric_type"]
		if !ok {
			log.Error("Missing required column 'metric_type' in custom query")
			return
		}
		metricTypeString, ok := metricTypeInterface.(string)
		if !ok {
			log.Error("Non-string type %T for custom query 'metric_type' column", metricTypeInterface)
			continue
		}
		metricType, err := metric.SourceTypeForName(metricTypeString)
		if err != nil {
			log.Error("Invalid metric type %s: %s", metricTypeString, err)
			continue
		}

		value, ok := row["metric_value"]
		if !ok {
			log.Error("Missing required column 'metric_type' in custom query")
			return
		}

		attributes := []attribute.Attribute{
			{Key: "displayName", Value: instance.Metadata.Name},
			{Key: "entityName", Value: instance.Metadata.Namespace + ":" + instance.Metadata.Name},
		}
		for k, v := range row {
			if k == "metric_name" || k == "metric_type" || k == "metric_value" {
				continue
			}

			var valString string
			switch v := v.(type) {
			case []byte:
				valString = string(v)
			case string:
				valString = v
			default:
				valString = fmt.Sprint(v)
			}

			attributes = append(attributes, attribute.Attribute{Key: k, Value: valString})
		}

		ms := instance.NewMetricSet("PgCustomQuerySample", attributes...)
		err = ms.SetMetric(name, value, metricType)
		if err != nil {
			log.Error("Failed to set metric: %s", err)
			continue
		}
	}
}
