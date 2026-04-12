//go:generate goversioninfo
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	queryperformancemonitoring "github.com/newrelic/nri-postgresql/src/query-performance-monitoring"

	"github.com/newrelic/infra-integrations-sdk/v3/integration"
	"github.com/newrelic/infra-integrations-sdk/v3/log"
	"github.com/newrelic/nri-postgresql/src/args"
	"github.com/newrelic/nri-postgresql/src/collection"
	"github.com/newrelic/nri-postgresql/src/connection"
	"github.com/newrelic/nri-postgresql/src/connection/shun"
	"github.com/newrelic/nri-postgresql/src/inventory"
	"github.com/newrelic/nri-postgresql/src/metrics"
)

const (
	integrationName = "com.newrelic.postgresql"
)

var (
	integrationVersion = "0.0.0"
	gitCommit          = ""
	buildDate          = ""
)

func main() {

	var args args.ArgumentList
	// Create Integration
	pgIntegration, err := integration.New(integrationName, integrationVersion, integration.Args(&args))
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}

	if args.ShowVersion {
		fmt.Printf(
			"New Relic %s integration Version: %s, Platform: %s, GoVersion: %s, GitCommit: %s, BuildDate: %s\n",
			strings.Title(strings.Replace(integrationName, "com.newrelic.", "", 1)),
			integrationVersion,
			fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
			runtime.Version(),
			gitCommit,
			buildDate)
		os.Exit(0)
	}

	// Setup logging with verbose
	log.SetupLogging(args.Verbose)

	// Validate arguments
	if err := args.Validate(); err != nil {
		log.Error("Configuration error for args %v: %s", args, err.Error())
		os.Exit(1)
	}

	connectionInfo := connection.DefaultConnectionInfo(&args)
	collectionList, collectionErr := collection.BuildCollectionList(args, connectionInfo)
	if collectionErr != nil {
		observabilityActive := args.EnableAvailabilityCheck || args.CollectConnectionTiming
		if !args.HasMetrics() || !observabilityActive {
			log.Error("Error creating list of entities to collect: %s", collectionErr)
			os.Exit(1)
		}
		// The DB appears to be down but observability flags are active.  Fall through
		// with an empty collection list so PopulateMetrics can still emit
		// db.available=0 and the explicit availability check sample before we exit.
		log.Warn("Error creating list of entities to collect: %s", collectionErr)
		collectionList = collection.DatabaseList{}
	}
	instance, err := pgIntegration.Entity(fmt.Sprintf("%s:%s", args.Hostname, args.Port), "pg-instance")
	if err != nil {
		log.Error("Error creating instance entity: %s", err.Error())
		os.Exit(1)
	}
	// Initialize shun manager for error-aware backoff. When configured, persistent
	// failures (auth, DNS, TLS) trigger exponential backoff to avoid hammering the
	// server on every 15-second cycle.
	var shunMgr *shun.Manager
	if args.ShunStateFilePath != "" {
		shunMgr = shun.NewManager(args.ShunStateFilePath, 15*time.Second, nil)
		shunMgr.LoadState()
		defer shunMgr.SaveState()
	}

	if args.HasMetrics() {
		obs := metrics.ObservabilityConfig{
			CollectConnectionTiming:    args.CollectConnectionTiming,
			EnableAvailabilityCheck:    args.EnableAvailabilityCheck,
			AvailabilityCheckQuery:     args.AvailabilityCheckQuery,
			AvailabilityCheckTimeoutMs: args.AvailabilityCheckTimeoutMs,
			CollectQueryTelemetry:      args.CollectQueryTelemetry,
		}
		metrics.PopulateMetrics(connectionInfo, collectionList, instance, pgIntegration, args.Pgbouncer, args.CollectDbLockMetrics, args.CollectBloatMetrics, args.CustomMetricsQuery, obs, shunMgr)
		if args.CustomMetricsConfig != "" {
			metrics.PopulateCustomMetricsFromFile(connectionInfo, args.CustomMetricsConfig, pgIntegration)
		}
	}

	if args.HasInventory() {
		con, err := connectionInfo.NewConnection(connectionInfo.DatabaseName())
		if err != nil {
			log.Error("Inventory collection failed: error creating connection to PostgreSQL: %s", err.Error())
		} else {
			defer con.Close()
			inventory.PopulateInventory(instance, con)
		}
	}

	if err = pgIntegration.Publish(); err != nil {
		log.Error(err.Error())
	}

	if collectionErr != nil {
		os.Exit(1)
	}

	if args.EnableQueryMonitoring {
		queryperformancemonitoring.QueryPerformanceMain(args, pgIntegration, collectionList)
	}

}
