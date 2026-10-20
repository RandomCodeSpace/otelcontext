package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

func TestSQLiteMainDBPathForAccounting(t *testing.T) {
	for _, tc := range []struct{ driver, dsn, want string }{
		{"sqlite", "", "OtelContext.db"},
		{"sqlite", ":memory:", ""},
		{"sqlite", "file::memory:?cache=shared", ""},
		{"sqlite", "file:memdb?mode=memory&cache=shared", ""},
		{"postgres", "host=localhost", ""},
		{"sqlite", "file:outside.db?mode=rwc", "outside.db"},
		{"sqlite", "file:/tmp/space%20name.db?mode=rwc", "/tmp/space name.db"},
		{"sqlite", "./outside.db?_busy_timeout=5000", "./outside.db"},
	} {
		t.Run(tc.driver+"/"+tc.dsn, func(t *testing.T) {
			got := sqliteMainDBPath(&config.Config{DBDriver: tc.driver, DBDSN: tc.dsn})
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestApplicationDiskUsageResolvesSQLiteSidecars(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	external := filepath.Join(root, "external")
	for _, dir := range []string{data, external} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	real := filepath.Join(external, "actual.db")
	link := filepath.Join(root, "configured.db")
	if err := os.WriteFile(real, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	db, err := storage.NewDatabase("sqlite", link)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	for _, stmt := range []string{"PRAGMA wal_autocheckpoint=0", "CREATE TABLE payload (value BLOB)", "INSERT INTO payload VALUES (zeroblob(1048576))"} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	var want int64
	for _, name := range []string{real, real + "-wal", real + "-shm"} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		want += info.Size()
	}
	for _, dsn := range []string{real, link, "file:" + link + "?mode=rwc"} {
		got, err := applicationDiskUsage(&config.Config{DBDriver: "sqlite", DBDSN: dsn}, data)
		if err != nil || got != want {
			t.Fatalf("dsn=%q got=%d want=%d err=%v", dsn, got, want, err)
		}
	}
}
