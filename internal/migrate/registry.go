// Package migrate owns OtelContext's ordered main-database schema contract.
package migrate

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	// LedgerTable is shared by main storage and GraphRAG migrations.
	LedgerTable = "otelcontext_schema_migrations"
	// CurrentVersion is the exact relational schema version required by this binary.
	CurrentVersion = 3
)

//go:embed sql/*/*.sql
var migrationSQL embed.FS

type migration struct {
	Version       int
	Name          string
	Transactional bool
	SQL           string
	Checksum      string
}

func registryFor(driver string) ([]migration, error) {
	driver = NormalizeDriver(driver)
	if !SupportsVersioned(driver) {
		return nil, fmt.Errorf("versioned migrations are not verified for driver %q", driver)
	}
	definitions := []struct {
		version int
		name    string
		file    string
	}{
		{1, "v0.3.1", "001_v0_3_1.sql"},
		{2, "v0.4.0-beta.2", "002_v0_4_0_beta_2.sql"},
		{3, "v0.4.0-rc.4", "003_v0_4_0_rc_4.sql"},
	}
	registry := make([]migration, 0, len(definitions))
	for _, definition := range definitions {
		path := fmt.Sprintf("sql/%s/%s", driver, definition.file)
		content, err := migrationSQL.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read embedded migration %s: %w", path, err)
		}
		sum := sha256.Sum256(content)
		registry = append(registry, migration{
			Version:       definition.version,
			Name:          definition.name,
			Transactional: true,
			SQL:           strings.TrimSpace(string(content)),
			Checksum:      hex.EncodeToString(sum[:]),
		})
	}
	if err := validateRegistry(registry); err != nil {
		return nil, err
	}
	return registry, nil
}

func validateRegistry(registry []migration) error {
	if len(registry) != CurrentVersion {
		return fmt.Errorf("migration registry has %d entries, expected %d", len(registry), CurrentVersion)
	}
	seen := make(map[int]struct{}, len(registry))
	for i, entry := range registry {
		want := i + 1
		if entry.Version != want {
			return fmt.Errorf("migration registry gap: entry %d has version %d", want, entry.Version)
		}
		if _, duplicate := seen[entry.Version]; duplicate {
			return fmt.Errorf("duplicate migration version %d", entry.Version)
		}
		seen[entry.Version] = struct{}{}
		if strings.TrimSpace(entry.Name) == "" || strings.TrimSpace(entry.SQL) == "" || len(entry.Checksum) != 64 {
			return fmt.Errorf("migration version %d has incomplete metadata", entry.Version)
		}
		if !entry.Transactional {
			return fmt.Errorf("migration version %d is non-transactional without a resume implementation", entry.Version)
		}
	}
	return nil
}

func migrationStatements(sqlText string) []string {
	parts := strings.Split(sqlText, "-- migrate:split")
	statements := make([]string, 0, len(parts))
	for _, part := range parts {
		statement := strings.TrimSpace(part)
		if statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements
}

// NormalizeDriver returns the canonical driver name used by the registry.
func NormalizeDriver(driver string) string {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "", "sqlite":
		return "sqlite"
	case "postgres", "postgresql":
		return "postgres"
	case "mssql", "sqlserver":
		return "sqlserver"
	default:
		return strings.ToLower(strings.TrimSpace(driver))
	}
}

// SupportsVersioned reports whether this driver has promoted migration definitions.
func SupportsVersioned(driver string) bool {
	switch NormalizeDriver(driver) {
	case "sqlite", "postgres":
		return true
	default:
		return false
	}
}
