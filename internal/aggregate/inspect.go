package aggregate

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// StoreInspection is a read-only aggregate compatibility result.
type StoreInspection struct {
	State                 string
	ExpectedSchemaVersion int
	ActualSchemaVersion   int
	ExpectedSeriesVersion int
	ActualSeriesVersion   int
	ExpectedSketchVersion int
	ActualSketchVersion   int
	StoreUUID             string
	MigrationResult       string
	Detail                string
}

// Usable reports whether this binary can safely open the aggregate store.
func (s StoreInspection) Usable() bool { return s.State == "exact" }

// Description returns a stable one-line representation for operator output.
func (s StoreInspection) Description() string {
	actual := "none"
	if s.ActualSchemaVersion != 0 {
		actual = strconv.Itoa(s.ActualSchemaVersion)
	}
	parts := []string{
		"state=" + s.State,
		fmt.Sprintf("expected=%d", s.ExpectedSchemaVersion),
		"actual=" + actual,
		fmt.Sprintf("series_key=%d/%d", s.ActualSeriesVersion, s.ExpectedSeriesVersion),
		fmt.Sprintf("sketch_codec=%d/%d", s.ActualSketchVersion, s.ExpectedSketchVersion),
	}
	if s.StoreUUID != "" {
		parts = append(parts, "store_uuid="+s.StoreUUID)
	}
	if s.MigrationResult != "" {
		parts = append(parts, "migration_result="+s.MigrationResult)
	}
	if s.Detail != "" {
		parts = append(parts, "detail="+fmt.Sprintf("%q", s.Detail))
	}
	return strings.Join(parts, " ")
}

// InspectSQLiteStore reads aggregate tables and metadata without creating,
// rebuilding, or changing the configured file.
func InspectSQLiteStore(path string) (StoreInspection, error) {
	if path == "" {
		path = DefaultAggregateDBPath
	}
	inspection := StoreInspection{
		ExpectedSchemaVersion: StoreSchemaVersion,
		ExpectedSeriesVersion: int(SeriesKeyVersion),
		ExpectedSketchVersion: int(SketchEncodingVersion),
		MigrationResult:       "none",
	}
	if path == ":memory:" || strings.HasPrefix(path, "file::memory:") {
		inspection.State = "empty"
		inspection.Detail = "an in-memory aggregate store has no durable schema to inspect"
		return inspection, nil
	}
	plainPath := path
	if strings.HasPrefix(path, "file:") {
		parsed, err := url.Parse(path)
		if err != nil {
			return inspection, fmt.Errorf("parse aggregate store path: %w", err)
		}
		plainPath = parsed.Path
	}
	if _, err := os.Stat(plainPath); err != nil {
		if os.IsNotExist(err) {
			inspection.State = "empty"
			inspection.Detail = "aggregate store file is absent"
			return inspection, nil
		}
		return inspection, fmt.Errorf("stat aggregate store: %w", err)
	}
	absolute, err := filepath.Abs(plainPath)
	if err != nil {
		return inspection, fmt.Errorf("resolve aggregate store path: %w", err)
	}
	readOnlyURI := (&url.URL{Scheme: "file", Path: absolute}).String() + "?mode=ro"
	db, err := sql.Open("sqlite", readOnlyURI)
	if err != nil {
		return inspection, fmt.Errorf("open aggregate store read-only: %w", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'aggregate_%'`)
	if err != nil {
		return inspection, fmt.Errorf("inspect aggregate tables: %w", err)
	}
	present := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return inspection, err
		}
		present[name] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return inspection, err
	}
	if len(present) == 0 {
		inspection.State = "empty"
		inspection.Detail = "aggregate store contains no aggregate schema"
		return inspection, nil
	}
	var missing []string
	for _, table := range aggregateTables {
		if _, ok := present[table]; !ok {
			missing = append(missing, table)
		}
	}
	if len(missing) != 0 {
		inspection.State = "incompatible"
		inspection.Detail = "partial aggregate schema is missing " + strings.Join(missing, ", ")
		return inspection, nil
	}
	metaRows, err := db.Query(`SELECT key, value FROM aggregate_meta`)
	if err != nil {
		return inspection, fmt.Errorf("read aggregate metadata: %w", err)
	}
	meta := make(map[string]string)
	for metaRows.Next() {
		var key, value string
		if err := metaRows.Scan(&key, &value); err != nil {
			_ = metaRows.Close()
			return inspection, err
		}
		meta[key] = value
	}
	if err := metaRows.Close(); err != nil {
		return inspection, err
	}
	inspection.ActualSchemaVersion, _ = strconv.Atoi(meta["schema_version"])
	inspection.ActualSeriesVersion, _ = strconv.Atoi(meta["series_key_version"])
	inspection.ActualSketchVersion, _ = strconv.Atoi(meta["sketch_codec_version"])
	inspection.StoreUUID = meta["store_uuid"]
	if meta["schema_version"] == "" || meta["series_key_version"] == "" || meta["sketch_codec_version"] == "" || inspection.StoreUUID == "" {
		inspection.State = "incompatible"
		inspection.Detail = "aggregate metadata is incomplete"
		return inspection, nil
	}
	if inspection.ActualSchemaVersion != inspection.ExpectedSchemaVersion {
		inspection.State = "incompatible"
		inspection.Detail = fmt.Sprintf("aggregate schema %d cannot be migrated losslessly to %d; run the older binary or explicitly rebuild", inspection.ActualSchemaVersion, inspection.ExpectedSchemaVersion)
		return inspection, nil
	}
	if inspection.ActualSeriesVersion != inspection.ExpectedSeriesVersion || inspection.ActualSketchVersion != inspection.ExpectedSketchVersion {
		inspection.State = "incompatible"
		inspection.Detail = "aggregate series-key or sketch-codec version does not match this binary"
		return inspection, nil
	}
	inspection.State = "exact"
	inspection.Detail = "aggregate schema and semantic codec versions are exact"
	return inspection, nil
}
