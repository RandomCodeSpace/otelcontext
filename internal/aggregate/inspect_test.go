package aggregate

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectSQLiteStoreMissingIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	status, err := InspectSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "empty" || status.Usable() {
		t.Fatalf("status = %#v", status)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("read-only inspection created %s: %v", path, err)
	}
}

func TestInspectSQLiteStoreReportsExactSemanticVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	store, err := OpenSQLiteStore(StoreConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	status, err := InspectSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Usable() || status.ActualSchemaVersion != StoreSchemaVersion || status.ActualSeriesVersion != int(SeriesKeyVersion) || status.ActualSketchVersion != int(SketchEncodingVersion) || status.StoreUUID == "" {
		t.Fatalf("status = %#v", status)
	}
	for _, marker := range []string{"state=exact", "series_key=1/1", "sketch_codec=1/1", "store_uuid="} {
		if !strings.Contains(status.Description(), marker) {
			t.Fatalf("description %q missing %q", status.Description(), marker)
		}
	}
}

func TestInspectSQLiteStoreRefusesUnknowableV4WithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate-v4.db")
	store, err := OpenSQLiteStore(StoreConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE aggregate_meta SET value='4' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	status, err := InspectSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "incompatible" || status.ActualSchemaVersion != 4 || !strings.Contains(status.Detail, "cannot be migrated losslessly") {
		t.Fatalf("status = %#v", status)
	}
	if string(before) != string(after) {
		t.Fatal("read-only inspection changed the v4 aggregate file")
	}
}
