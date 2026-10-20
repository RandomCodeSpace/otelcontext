// Command rawcapacity is the production-repository side of simulate.py.
// It only opens a database in a directory created by its init action.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/migrate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

const marker = "synthetic raw capacity fixture v1\n"

func main() {
	action := flag.String("action", "", "init, purge, roll, read, cancel, or validate")
	dir := flag.String("dir", "", "owned fixture directory; init requires a NEW directory")
	cutoffText := flag.String("cutoff", "", "explicit simulated retention cutoff, RFC3339")
	id := flag.String("trace", "", "trace ID for repository read")
	appendDB := flag.String("append-db", "", "owned one-hour input database for roll")
	flag.Parse()
	result, err := run(*action, *dir, *cutoffText, *id, *appendDB)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(action, dir, cutoffText, id, appendDB string) (any, error) {
	if dir == "" {
		return nil, errors.New("fixture directory required")
	}
	var cutoff time.Time
	var err error
	switch action {
	case "init", "read", "validate":
	case "purge", "roll", "cancel":
		cutoff, err = time.Parse(time.RFC3339, cutoffText)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("unknown action")
	}
	if action == "init" {
		if err := os.Mkdir(dir, 0o750); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, ".rawcapacity"), []byte(marker), 0o640); err != nil {
			return nil, err
		}
	} else {
		b, err := os.ReadFile(filepath.Join(dir, ".rawcapacity"))
		if err != nil || string(b) != marker {
			return nil, errors.New("refusing database without rawcapacity ownership marker")
		}
	}
	path := filepath.Join(dir, "raw.db")
	if action != "init" {
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() {
			return nil, errors.New("fixture database must be an existing regular file")
		}
	}
	db, err := storage.NewDatabase("sqlite", path)
	if err != nil {
		return nil, err
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	defer repo.Close()
	ctx := context.Background()
	if action == "init" {
		if err := os.Setenv("LOG_FTS_ENABLED", "true"); err != nil {
			return nil, err
		}
		if _, err := migrate.Up(ctx, db, "sqlite"); err != nil {
			return nil, err
		}
		var n int64
		if err := db.Raw("SELECT count(*) FROM sqlite_master WHERE name='logs_fts'").Scan(&n).Error; err != nil || n != 1 {
			return nil, fmt.Errorf("FTS provisioning failed: count=%d error=%v", n, err)
		}
	}
	if action == "validate" {
		// Connection tuning can write the SQLite header even on a read path.
		// Isolate that startup work before measuring migration's no-op behavior.
		if err := repo.CheckpointWAL(ctx); err != nil {
			return nil, err
		}
		bytesBefore, err := databaseFingerprint(path)
		if err != nil {
			return nil, err
		}
		var before, after, schemaBefore, schemaAfter int64
		for query, dst := range map[string]*int64{"SELECT total_changes()": &before, "PRAGMA schema_version": &schemaBefore} {
			if err := db.Raw(query).Scan(dst).Error; err != nil {
				return nil, err
			}
		}
		start := time.Now()
		status, err := migrate.Up(ctx, db, "sqlite")
		if err != nil {
			return nil, err
		}
		upSeconds := time.Since(start).Seconds()
		if err := db.Exec("PRAGMA query_only=ON").Error; err != nil {
			return nil, err
		}
		if _, err := migrate.RequireExact(ctx, db, "sqlite"); err != nil {
			return nil, err
		}
		for query, dst := range map[string]*int64{"SELECT total_changes()": &after, "PRAGMA schema_version": &schemaAfter} {
			if err := db.Raw(query).Scan(dst).Error; err != nil {
				return nil, err
			}
		}
		if before != after || schemaBefore != schemaAfter {
			return nil, errors.New("complete migration changed data or schema")
		}
		bytesAfter, err := databaseFingerprint(path)
		if err != nil {
			return nil, err
		}
		for file, hash := range bytesBefore {
			if bytesAfter[file] != hash {
				return nil, errors.New("complete migration changed database or WAL bytes")
			}
		}
		var version string
		if err := db.Raw("SELECT sqlite_version()").Scan(&version).Error; err != nil {
			return nil, err
		}
		return map[string]any{"total_changes_before": before, "total_changes_after": after,
			"schema_before": schemaBefore, "schema_after": schemaAfter, "version": status.ActualVersion,
			"sha256_before_up": bytesBefore, "sha256_after_require_exact": bytesAfter,
			"sqlite_version": version, "up_seconds": upSeconds, "require_exact_query_only": true}, nil
	}
	if _, err := migrate.RequireExact(ctx, db, "sqlite"); err != nil {
		return nil, err
	}
	switch action {
	case "init":
		return map[string]any{"database": path, "fts": true}, nil
	case "roll":
		return roll(ctx, db, repo, cutoff, appendDB)
	case "read":
		trace, err := repo.GetTrace(ctx, id)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return map[string]any{"found": false}, nil
		}
		if err != nil {
			return nil, err
		}
		logs, total, err := repo.GetLogsV2(ctx, storage.LogFilter{TraceID: id, Limit: 100})
		if err != nil {
			return nil, err
		}
		var searchTotal int64
		if len(logs) > 0 && len(strings.Fields(logs[0].Body)) > 0 {
			_, searchTotal, err = repo.GetLogsV2(ctx, storage.LogFilter{TraceID: id, Search: strings.Fields(logs[0].Body)[0], Limit: 100})
			if err != nil {
				return nil, err
			}
		}
		return map[string]any{"found": true, "trace": trace, "log_read_count": len(logs), "log_total": total, "fts_log_total": searchTotal}, nil
	default:
		if action == "cancel" {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
		}
		start := time.Now()
		fmt.Fprintln(os.Stdout, `{"purge_started":true}`)
		stats, err := repo.PurgeExemplarsBatched(ctx, cutoff, 10000, 5*time.Millisecond)
		if action == "cancel" && errors.Is(err, context.Canceled) && stats.Total() == 0 {
			return map[string]any{"cancelled": true, "deleted": 0}, nil
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"cutoff": cutoff, "stats": stats, "seconds": time.Since(start).Seconds()}, nil
	}
}

func databaseFingerprint(path string) (map[string]string, error) {
	result := map[string]string{}
	for _, suffix := range []string{"", "-wal"} {
		hash := sha256.New()
		file, err := os.Open(path + suffix)
		if err != nil && !(suffix == "-wal" && errors.Is(err, os.ErrNotExist)) {
			return nil, err
		}
		if err == nil {
			_, err = io.Copy(hash, file)
			closeErr := file.Close()
			if err != nil {
				return nil, err
			}
			if closeErr != nil {
				return nil, closeErr
			}
		}
		result[suffix] = fmt.Sprintf("%x", hash.Sum(nil))
	}
	return result, nil
}

// roll shares the application's single-connection pool between append and
// purge. Two independently opened writers do not model this production path:
// FTS transactions can otherwise fail with SQLITE_BUSY_SNAPSHOT on upgrade.
func roll(ctx context.Context, db *gorm.DB, repo *storage.Repository, cutoff time.Time, input string) (any, error) {
	fi, err := os.Lstat(input)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("append input must be an existing regular file")
	}
	abs, err := filepath.Abs(input)
	if err != nil {
		return nil, err
	}
	uri := (&url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro&immutable=1"}).String()
	if err := db.Exec("ATTACH DATABASE ? AS capacity_append", uri).Error; err != nil {
		return nil, err
	}
	defer db.Exec("DETACH DATABASE capacity_append")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		stats   storage.ExemplarPurgeStats
		seconds float64
		err     error
	}
	done := make(chan result, 1)
	started := make(chan struct{})
	var finished atomic.Bool
	go func() {
		start := time.Now()
		close(started)
		stats, err := repo.PurgeExemplarsBatched(ctx, cutoff, 10000, 5*time.Millisecond)
		finished.Store(true)
		done <- result{stats, time.Since(start).Seconds(), err}
	}()
	<-started
	appendStart := time.Now()
	counts := map[string]int64{}
	batchesDuringPurge := 0
	appendErr := func() error {
		for _, table := range []string{"traces", "spans", "logs"} {
			counts[table] = 0
			var columns []struct{ Name string }
			if err := db.Raw("PRAGMA main.table_info(" + table + ")").Scan(&columns).Error; err != nil {
				return err
			}
			var names []string
			for _, column := range columns {
				if column.Name != "id" {
					names = append(names, `"`+strings.ReplaceAll(column.Name, `"`, `""`)+`"`)
				}
			}
			var maximum int64
			if err := db.Raw("SELECT coalesce(max(id),0) FROM capacity_append." + table).Scan(&maximum).Error; err != nil {
				return err
			}
			cols := strings.Join(names, ",")
			query := fmt.Sprintf("INSERT INTO main.%s (%s) SELECT %s FROM capacity_append.%s WHERE id > ? AND id <= ? ORDER BY id", table, cols, cols, table)
			for offset := int64(0); offset < maximum; offset += 500 {
				res := db.WithContext(ctx).Exec(query, offset, offset+500)
				if res.Error != nil {
					return res.Error
				}
				counts[table] += res.RowsAffected
				if !finished.Load() {
					batchesDuringPurge++
				}
			}
		}
		return nil
	}()
	appendSeconds := time.Since(appendStart).Seconds()
	if appendErr != nil {
		cancel()
	}
	purge := <-done
	if appendErr != nil {
		return nil, appendErr
	}
	if purge.err != nil {
		return nil, purge.err
	}
	return map[string]any{"appended": counts, "append_seconds": appendSeconds,
		"append_batches_before_purge_finish": batchesDuringPurge,
		"purge":                              map[string]any{"cutoff": cutoff, "stats": purge.stats, "seconds": purge.seconds}}, nil
}
