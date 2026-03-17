package archive

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// Maintain runs driver-specific DB optimization commands after archival.
// SQLite: VACUUM + PRAGMA optimize
// PostgreSQL: VACUUM ANALYZE
// MySQL: OPTIMIZE TABLE for each OtelContext table
func Maintain(repo *storage.Repository, cfg *config.Config) error {
	db := repo.DB()
	driver := strings.ToLower(cfg.DBDriver)

	switch driver {
	case "sqlite", "":
		slog.Info("🔧 Running SQLite maintenance (VACUUM + PRAGMA optimize)")
		if err := db.Exec("PRAGMA optimize").Error; err != nil {
			return fmt.Errorf("PRAGMA optimize failed: %w", err)
		}
		if err := db.Exec("VACUUM").Error; err != nil {
			return fmt.Errorf("VACUUM failed: %w", err)
		}

	case "postgres", "postgresql":
		slog.Info("🔧 Running PostgreSQL maintenance (VACUUM ANALYZE)")
		for _, table := range []string{"traces", "spans", "logs", "metric_buckets"} {
			if err := db.Exec(fmt.Sprintf("VACUUM ANALYZE %s", table)).Error; err != nil {
				return fmt.Errorf("VACUUM ANALYZE %s failed: %w", table, err)
			}
		}

	case "mysql":
		slog.Info("🔧 Running MySQL maintenance (OPTIMIZE TABLE)")
		for _, table := range []string{"traces", "spans", "logs", "metric_buckets"} {
			if err := db.Exec(fmt.Sprintf("OPTIMIZE TABLE %s", table)).Error; err != nil {
				return fmt.Errorf("OPTIMIZE TABLE %s failed: %w", table, err)
			}
		}
	}

	slog.Info("✅ DB maintenance complete")
	return nil
}

