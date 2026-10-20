package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOwnershipAndProductionSetup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fixture")
	if _, err := run("purge", dir, "2026-01-01T00:00:00Z", "", ""); err == nil {
		t.Fatal("accepted unowned database")
	}
	if _, err := run("init", dir, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := run("init", dir, "", "", ""); err == nil {
		t.Fatal("accepted existing directory")
	}
	if _, err := run("validate", dir, "", "", ""); err != nil {
		t.Fatal("complete production FTS migration must not write:", err)
	}
	result, err := run("cancel", dir, "2026-01-01T00:00:00Z", "", "")
	if err != nil || !result.(map[string]any)["cancelled"].(bool) {
		t.Fatalf("production purge cancellation: %v %v", result, err)
	}
	if err := os.Rename(filepath.Join(dir, "raw.db"), filepath.Join(dir, "protected.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("protected.db", filepath.Join(dir, "raw.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := run("purge", dir, "2026-01-01T00:00:00Z", "", ""); err == nil {
		t.Fatal("accepted symlink database")
	}
}
