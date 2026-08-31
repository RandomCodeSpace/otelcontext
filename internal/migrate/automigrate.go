package migrate

import (
	"fmt"

	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

// AutoMigrate is the single development and preview-driver schema owner.
// It deliberately does not stamp the versioned ledger: operators must validate
// and baseline a database before changing to DB_AUTOMIGRATE=false.
func AutoMigrate(db *gorm.DB, driver string, options storage.MigrateOptions) error {
	if err := storage.AutoMigrateModelsWithOptions(db, driver, options); err != nil {
		return err
	}
	if err := graphrag.AutoMigrateGraphRAG(db); err != nil {
		return fmt.Errorf("migrate GraphRAG schema: %w", err)
	}
	return nil
}
