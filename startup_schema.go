package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/config"
	schemamigrate "github.com/RandomCodeSpace/otelcontext/internal/migrate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// prepareDatabaseSchemas is the startup quiescence gate for schema ownership.
// The caller must invoke it before starting any worker or listener.
func prepareDatabaseSchemas(ctx context.Context, cfg *config.Config, repo *storage.Repository) error {
	autoMigrate := storage.AutoMigrateEnabled()
	if autoMigrate {
		options := storage.MigrateOptionsFromEnv()
		if err := schemamigrate.AutoMigrate(repo.DB(), cfg.DBDriver, options); err != nil {
			return fmt.Errorf("development AutoMigrate: %w", err)
		}
		if schemamigrate.NormalizeDriver(cfg.DBDriver) == "postgres" && options.PostgresPartitioning == storage.PartitioningModeDaily {
			repo.MarkLogsPartitioned()
		}
		slog.Info("Development AutoMigrate completed for main storage and GraphRAG",
			"driver", schemamigrate.NormalizeDriver(cfg.DBDriver),
			"binary_version", Version,
		)
	} else if schemamigrate.SupportsVersioned(cfg.DBDriver) {
		if err := versionedProfileError(cfg); err != nil {
			return err
		}
		status, err := schemamigrate.RequireExact(ctx, repo.DB(), cfg.DBDriver)
		slog.Info("Production schema compatibility check",
			"driver", status.Driver,
			"state", status.State,
			"expected_version", status.ExpectedVersion,
			"actual_version", status.ActualVersion,
			"fingerprint", status.Fingerprint,
			"binary_version", Version,
		)
		if err != nil {
			return err
		}
	} else {
		slog.Warn("DB_AUTOMIGRATE=false leaves schema compatibility unverified for preview driver",
			"driver", schemamigrate.NormalizeDriver(cfg.DBDriver),
			"binary_version", Version,
			"action", "keep DB_AUTOMIGRATE=true until versioned definitions are promoted",
		)
	}

	if !autoMigrate && cfg.AggregateMode != aggregate.ModeLegacy {
		status, err := aggregate.InspectSQLiteStore(cfg.AggregateDBPath)
		if err != nil {
			return fmt.Errorf("inspect aggregate schema: %w", err)
		}
		if !status.Usable() && cfg.AggregateAllowRebuild {
			store, rebuildErr := aggregate.OpenSQLiteStore(aggregate.StoreConfig{
				Path:         cfg.AggregateDBPath,
				AllowRebuild: true,
			})
			if rebuildErr != nil {
				return fmt.Errorf("explicit aggregate rebuild: %w", rebuildErr)
			}
			if closeErr := store.Close(); closeErr != nil {
				return fmt.Errorf("close explicitly rebuilt aggregate store: %w", closeErr)
			}
			status, err = aggregate.InspectSQLiteStore(cfg.AggregateDBPath)
			if err != nil {
				return fmt.Errorf("inspect explicitly rebuilt aggregate schema: %w", err)
			}
		}
		slog.Info("Production aggregate compatibility check",
			"state", status.State,
			"expected_version", status.ExpectedSchemaVersion,
			"actual_version", status.ActualSchemaVersion,
			"series_key_version", status.ActualSeriesVersion,
			"sketch_codec_version", status.ActualSketchVersion,
			"store_uuid", status.StoreUUID,
			"binary_version", Version,
		)
		if !status.Usable() {
			return fmt.Errorf("startup refused: aggregate schema state=%s expected=%d actual=%d: %s; run `otelcontext migrate status` then `otelcontext migrate up`, use the older signed binary, or explicitly rebuild with AGGREGATE_ALLOW_REBUILD=true",
				status.State, status.ExpectedSchemaVersion, status.ActualSchemaVersion, status.Detail)
		}
	}
	return nil
}
