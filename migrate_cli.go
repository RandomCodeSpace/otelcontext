package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/config"
	schemamigrate "github.com/RandomCodeSpace/otelcontext/internal/migrate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

const migrationUsage = "usage: otelcontext migrate <status|up|baseline --from RELEASE>"

func maybeRunMigrationCommand(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 || args[0] != "migrate" {
		return false, 0
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, migrationUsage)
		return true, schemamigrate.ExitUsage
	}
	command := args[1]
	baselineFrom := ""
	switch command {
	case "status", "up":
		if len(args) != 2 {
			fmt.Fprintln(stderr, migrationUsage)
			return true, schemamigrate.ExitUsage
		}
	case "baseline":
		flags := flag.NewFlagSet("migrate baseline", flag.ContinueOnError)
		flags.SetOutput(stderr)
		from := flags.String("from", "", "published release to validate and record")
		if err := flags.Parse(args[2:]); err != nil || *from == "" || flags.NArg() != 0 {
			fmt.Fprintln(stderr, migrationUsage)
			return true, schemamigrate.ExitUsage
		}
		baselineFrom = *from
	default:
		fmt.Fprintln(stderr, migrationUsage)
		return true, schemamigrate.ExitUsage
	}
	ctx := context.Background()
	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(stderr, "migrate: load configuration: %v\n", err)
		return true, schemamigrate.ExitUsage
	}
	if err := validateMigrationConfig(cfg); err != nil {
		fmt.Fprintf(stderr, "migrate: %v\n", err)
		return true, schemamigrate.ExitUsage
	}
	if command != "status" {
		if err := versionedProfileError(cfg); err != nil {
			fmt.Fprintf(stderr, "migrate %s: %v\n", command, err)
			return true, schemamigrate.ExitIncompatible
		}
	}
	db, err := storage.NewDatabase(cfg.DBDriver, cfg.DBDSN)
	if err != nil {
		fmt.Fprintf(stderr, "migrate: open main database: %v\n", err)
		return true, schemamigrate.ExitIncompatible
	}
	if sqlDB, dbErr := db.DB(); dbErr == nil {
		defer func() { _ = sqlDB.Close() }()
	}

	switch command {
	case "status":
		return true, printMigrationStatus(ctx, stdout, stderr, cfg, db)
	case "up":
		return true, runMigrationUp(ctx, stdout, stderr, cfg, db)
	case "baseline":
		return true, runMigrationBaseline(ctx, stdout, stderr, cfg, db, baselineFrom)
	}
	return true, schemamigrate.ExitUsage
}

func validateMigrationConfig(cfg *config.Config) error {
	switch strings.ToLower(cfg.AggregateMode) {
	case aggregate.ModeLegacy, aggregate.ModeShadow, aggregate.ModeAggregate:
	default:
		return fmt.Errorf("invalid AGGREGATE_MODE %q", cfg.AggregateMode)
	}
	if cfg.AggregateMode != aggregate.ModeLegacy && strings.TrimSpace(cfg.AggregateDBPath) == "" {
		return fmt.Errorf("AGGREGATE_DB_PATH is required when AGGREGATE_MODE=%s", cfg.AggregateMode)
	}
	return nil
}

func printMigrationStatus(ctx context.Context, stdout, stderr io.Writer, cfg *config.Config, db *gorm.DB) int {
	mainStatus, err := schemamigrate.Inspect(ctx, db, cfg.DBDriver)
	if err != nil {
		fmt.Fprintf(stderr, "migrate status: inspect main database: %v\n", err)
		return schemamigrate.ExitIncompatible
	}
	if profileErr := versionedProfileError(cfg); profileErr != nil {
		mainStatus.State = schemamigrate.StateIncompatible
		mainStatus.Detail = profileErr.Error()
	}
	aggregateStatus, aggregateRequired, err := inspectConfiguredAggregate(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "migrate status: inspect aggregate database: %v\n", err)
		return schemamigrate.ExitIncompatible
	}
	printStatusReport(stdout, cfg, mainStatus, aggregateStatus, aggregateRequired)
	if code := mainStatus.ExitCode(); code != schemamigrate.ExitOK {
		return code
	}
	if aggregateRequired && !aggregateStatus.Usable() {
		if aggregateStatus.State == "empty" {
			return schemamigrate.ExitEmpty
		}
		return schemamigrate.ExitIncompatible
	}
	return schemamigrate.ExitOK
}

func runMigrationUp(ctx context.Context, stdout, stderr io.Writer, cfg *config.Config, db *gorm.DB) int {
	if err := versionedProfileError(cfg); err != nil {
		fmt.Fprintf(stderr, "migrate up: %v\n", err)
		return schemamigrate.ExitIncompatible
	}
	mainStatus, err := schemamigrate.Up(ctx, db, cfg.DBDriver)
	if err != nil {
		fmt.Fprintf(stderr, "migrate up: %v\n", err)
		if mainStatus.State != "" {
			fmt.Fprintf(stdout, "main %s\n", mainStatus.Description())
			var stateErr *schemamigrate.StateError
			if errors.As(err, &stateErr) {
				return mainStatus.ExitCode()
			}
		}
		return schemamigrate.ExitIncompatible
	}
	aggregateStatus, aggregateRequired, err := ensureConfiguredAggregate(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "migrate up: aggregate store: %v\n", err)
		fmt.Fprintf(stdout, "main %s\n", mainStatus.Description())
		if aggregateStatus.State != "" {
			fmt.Fprintf(stdout, "aggregate %s\n", aggregateStatus.Description())
		}
		return schemamigrate.ExitIncompatible
	}
	printStatusReport(stdout, cfg, mainStatus, aggregateStatus, aggregateRequired)
	return schemamigrate.ExitOK
}

func versionedProfileError(cfg *config.Config) error {
	if schemamigrate.NormalizeDriver(cfg.DBDriver) == "postgres" && cfg.DBPostgresPartitioning == storage.PartitioningModeDaily {
		return fmt.Errorf("DB_POSTGRES_PARTITIONING=daily is not promoted for versioned migrations; use the unpartitioned PostgreSQL 16 profile or keep DB_AUTOMIGRATE=true")
	}
	return nil
}

func runMigrationBaseline(ctx context.Context, stdout, stderr io.Writer, cfg *config.Config, db *gorm.DB, release string) int {
	if err := versionedProfileError(cfg); err != nil {
		fmt.Fprintf(stderr, "migrate baseline: %v\n", err)
		return schemamigrate.ExitIncompatible
	}
	result, err := schemamigrate.Baseline(ctx, db, cfg.DBDriver, release)
	if err != nil {
		fmt.Fprintf(stderr, "migrate baseline: %v\n", err)
		if result.Status.State != "" {
			fmt.Fprintf(stdout, "main %s\n", result.Status.Description())
			var stateErr *schemamigrate.StateError
			if errors.As(err, &stateErr) {
				return result.Status.ExitCode()
			}
		}
		return schemamigrate.ExitIncompatible
	}
	fmt.Fprintf(stdout, "binary=%s\n", Version)
	fmt.Fprintf(stdout, "driver=%s\n", schemamigrate.NormalizeDriver(cfg.DBDriver))
	fmt.Fprintf(stdout, "baseline release=%s recorded=%d before_fingerprint=%s after_fingerprint=%s\n",
		result.Release, result.RecordedVersion, result.BeforeFingerprint, result.AfterFingerprint)
	fmt.Fprintf(stdout, "main %s\n", result.Status.Description())
	if result.Status.State == schemamigrate.StateExact {
		fmt.Fprintln(stdout, "result=ready")
	} else {
		fmt.Fprintln(stdout, "result=baseline-recorded-migrate-up-required")
	}
	return schemamigrate.ExitOK
}

func inspectConfiguredAggregate(cfg *config.Config) (aggregate.StoreInspection, bool, error) {
	if cfg.AggregateMode == aggregate.ModeLegacy {
		return aggregate.StoreInspection{State: "not-required", ExpectedSchemaVersion: aggregate.StoreSchemaVersion, MigrationResult: "not-required"}, false, nil
	}
	status, err := aggregate.InspectSQLiteStore(cfg.AggregateDBPath)
	return status, true, err
}

func ensureConfiguredAggregate(cfg *config.Config) (aggregate.StoreInspection, bool, error) {
	status, required, err := inspectConfiguredAggregate(cfg)
	if err != nil || !required || status.Usable() {
		return status, required, err
	}
	if status.State != "empty" {
		return status, required, fmt.Errorf("state=%s: %s", status.State, status.Detail)
	}
	store, err := aggregate.OpenSQLiteStore(aggregate.StoreConfig{
		Path:         cfg.AggregateDBPath,
		AllowRebuild: false,
	})
	if err != nil {
		return status, required, err
	}
	if err := store.Close(); err != nil {
		return status, required, err
	}
	status, err = aggregate.InspectSQLiteStore(cfg.AggregateDBPath)
	status.MigrationResult = "created-v5"
	return status, required, err
}

func printStatusReport(stdout io.Writer, cfg *config.Config, mainStatus schemamigrate.Status, aggregateStatus aggregate.StoreInspection, aggregateRequired bool) {
	fmt.Fprintf(stdout, "binary=%s\n", Version)
	fmt.Fprintf(stdout, "driver=%s\n", schemamigrate.NormalizeDriver(cfg.DBDriver))
	fmt.Fprintf(stdout, "main %s\n", mainStatus.Description())
	if aggregateRequired {
		fmt.Fprintf(stdout, "aggregate %s\n", aggregateStatus.Description())
	} else {
		fmt.Fprintf(stdout, "aggregate state=not-required expected=%d actual=none migration_result=not-required\n", aggregate.StoreSchemaVersion)
	}
	if mainStatus.Usable() && (!aggregateRequired || aggregateStatus.Usable()) {
		fmt.Fprintln(stdout, "result=ready")
	} else {
		fmt.Fprintln(stdout, "result=action-required")
	}
}
