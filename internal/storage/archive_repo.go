package storage

// HotDBSizeBytes returns an approximate size of the hot DB in bytes.
// For SQLite this reads the file size. For others it queries pg_database_size / information_schema.
func (r *Repository) HotDBSizeBytes() int64 {
	rdr := r.reader()
	switch r.driver {
	case "sqlite", "":
		var pageCount, pageSize int64
		rdr.Raw("PRAGMA page_count").Scan(&pageCount)
		rdr.Raw("PRAGMA page_size").Scan(&pageSize)
		return pageCount * pageSize

	case "postgres", "postgresql":
		var size int64
		rdr.Raw("SELECT pg_database_size(current_database())").Scan(&size)
		return size

	case "mysql":
		var size int64
		rdr.Raw(`SELECT SUM(data_length + index_length) FROM information_schema.tables
			WHERE table_schema = DATABASE()`).Scan(&size)
		return size

	default:
		return 0
	}
}
