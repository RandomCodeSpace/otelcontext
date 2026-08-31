package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	mysqlcfg "github.com/go-sql-driver/mysql"
	"github.com/microsoft/go-mssqldb/msdsn"
)

func mainArtifactName(driver string) string {
	switch driver {
	case "sqlite":
		return "main.sqlite"
	case "postgres":
		return "main.postgres.dump"
	case "mysql":
		return "main.mysql.sql"
	case "mssql":
		return "main.sqlserver.bak"
	default:
		return "main.database"
	}
}

func captureMainDatabase(ctx context.Context, cfg Config, target string, runner CommandRunner, records *[]CommandRecord) (string, error) {
	driver := normalizeDriver(cfg.DBDriver)
	switch driver {
	case "sqlite":
		if err := vacuumSQLite(ctx, cfg.DBDSN, target); err != nil {
			return "", err
		}
		if err := sqliteIntegrity(ctx, target); err != nil {
			return "", err
		}
		return "PRAGMA integrity_check=ok; PRAGMA foreign_key_check=0 rows", nil
	case "postgres":
		return capturePostgres(ctx, cfg.DBDSN, target, runner, records)
	case "mysql":
		return captureMySQL(ctx, cfg.DBDSN, target, runner, records)
	case "mssql":
		return captureMSSQL(ctx, cfg.DBDSN, target, runner, records)
	default:
		return "", fmt.Errorf("unsupported backup adapter %q", driver)
	}
}

func restoreMainDatabase(ctx context.Context, driver, source, targetDSN string, runner CommandRunner, records *[]CommandRecord) error {
	switch driver {
	case "sqlite":
		target, err := sqlitePath(targetDSN)
		if err != nil {
			return err
		}
		return publishSQLiteCopy(source, target)
	case "postgres":
		if err := ensureFreshServerDatabase(ctx, driver, targetDSN); err != nil {
			return err
		}
		return restorePostgres(ctx, source, targetDSN, runner, records)
	case "mysql":
		if err := ensureFreshServerDatabase(ctx, driver, targetDSN); err != nil {
			return err
		}
		return restoreMySQL(ctx, source, targetDSN, runner, records)
	case "mssql":
		return restoreMSSQL(ctx, source, targetDSN, runner, records)
	default:
		return fmt.Errorf("unsupported restore adapter %q", driver)
	}
}

func verifyMainArtifact(ctx context.Context, driver, source, targetDSN string, runner CommandRunner, records *[]CommandRecord) error {
	switch driver {
	case "sqlite":
		return sqliteIntegrity(ctx, source)
	case "postgres":
		if err := requireVersion(ctx, runner, "pg_restore", []string{"--version"}, "pg_restore --version", " 16.", records, targetDSN); err != nil {
			return err
		}
		_, err := runRecorded(ctx, runner, "verify-bundle-main", Command{
			Name:       "pg_restore",
			Args:       []string{"--list", source},
			Display:    "pg_restore --list <bundle>/main.postgres.dump",
			Redactions: []string{targetDSN},
		}, records)
		return err
	case "mysql":
		if _, err := requireRegular(source); err != nil {
			return err
		}
		file, err := os.Open(source) // #nosec G304 -- verified bundle artifact.
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		data := make([]byte, 1<<20)
		read, err := file.Read(data)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		data = data[:read]
		if len(data) == 0 || (!strings.Contains(string(data), "CREATE TABLE") && !strings.Contains(string(data), "INSERT INTO")) {
			return errors.New("MySQL bundle artifact is not a recognizable logical dump")
		}
		return nil
	case "mssql":
		cfg, err := parseMSSQLDSN(targetDSN)
		if err != nil {
			return err
		}
		if err := requireVersion(ctx, runner, "sqlcmd", []string{"-?"}, "sqlcmd -?", "Version 18.", records, cfg.Password, targetDSN); err != nil {
			return err
		}
		base, redactions := sqlcmdBase(cfg)
		query := fmt.Sprintf("SET NOCOUNT ON; RESTORE VERIFYONLY FROM DISK = N'%s' WITH CHECKSUM;", mssqlLiteral(source))
		_, err = runRecorded(ctx, runner, "verify-bundle-main", Command{
			Name:       "sqlcmd",
			Args:       append(append([]string{}, base...), "-Q", query),
			Display:    "sqlcmd <server> -d master -Q RESTORE VERIFYONLY WITH CHECKSUM",
			Redactions: append(redactions, targetDSN),
		}, records)
		return err
	default:
		return fmt.Errorf("unsupported backup adapter %q", driver)
	}
}

func validateFreshNativeTarget(ctx context.Context, driver, dsn string, runner CommandRunner) error {
	if driver != "mssql" {
		return ensureFreshServerDatabase(ctx, driver, dsn)
	}
	cfg, err := parseMSSQLDSN(dsn)
	if err != nil {
		return err
	}
	base, redactions := sqlcmdBase(cfg)
	query := fmt.Sprintf("SET NOCOUNT ON; SELECT CONVERT(varchar(128),SERVERPROPERTY('ProductVersion')) + '|' + CASE WHEN DB_ID(N'%s') IS NULL THEN 'missing' ELSE 'exists' END;", mssqlLiteral(cfg.Database))
	var records []CommandRecord
	result, err := runRecorded(ctx, runner, "validate-fresh-target", Command{
		Name:       "sqlcmd",
		Args:       append(append([]string{}, base...), "-W", "-h", "-1", "-Q", query),
		Display:    "sqlcmd <server> -d master -Q validate fresh target",
		Redactions: append(redactions, dsn),
	}, &records)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(result.Output, "\n") {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) != 2 {
			continue
		}
		if err := validateEngineProfile("mssql", strings.TrimSpace(parts[0])); err != nil {
			return err
		}
		if strings.TrimSpace(parts[1]) != "missing" {
			return fmt.Errorf("restore target is not fresh: SQL Server database %q already exists", cfg.Database)
		}
		return nil
	}
	return errors.New("SQL Server fresh-target query returned no engine version or database state")
}

func capturePostgres(ctx context.Context, dsn, target string, runner CommandRunner, records *[]CommandRecord) (string, error) {
	if err := requireVersion(ctx, runner, "pg_dump", []string{"--version"}, "pg_dump --version", " 16.", records, dsn); err != nil {
		return "", err
	}
	if err := requireVersion(ctx, runner, "pg_restore", []string{"--version"}, "pg_restore --version", " 16.", records, dsn); err != nil {
		return "", err
	}
	_, err := runRecorded(ctx, runner, "capture-main", Command{
		Name: "pg_dump",
		Args: []string{
			"--format=custom",
			"--no-owner",
			"--no-privileges",
			"--file=" + target,
			"--dbname=" + dsn,
		},
		Display:    "pg_dump --format=custom --no-owner --no-privileges --file=<bundle>/main.postgres.dump --dbname=[redacted]",
		Redactions: []string{dsn},
	}, records)
	if err != nil {
		return "", err
	}
	if _, err := requireRegular(target); err != nil {
		return "", fmt.Errorf("pg_dump did not create its artifact: %w", err)
	}
	_, err = runRecorded(ctx, runner, "integrity-main", Command{
		Name:       "pg_restore",
		Args:       []string{"--list", target},
		Display:    "pg_restore --list <bundle>/main.postgres.dump",
		Redactions: []string{dsn},
	}, records)
	if err != nil {
		return "", err
	}
	return "PostgreSQL 16 custom archive parsed by pg_restore --list", nil
}

func restorePostgres(ctx context.Context, source, dsn string, runner CommandRunner, records *[]CommandRecord) error {
	if err := requireVersion(ctx, runner, "pg_restore", []string{"--version"}, "pg_restore --version", " 16.", records, dsn); err != nil {
		return err
	}
	_, err := runRecorded(ctx, runner, "restore-main", Command{
		Name: "pg_restore",
		Args: []string{
			"--exit-on-error",
			"--no-owner",
			"--no-privileges",
			"--dbname=" + dsn,
			source,
		},
		Display:    "pg_restore --exit-on-error --no-owner --no-privileges --dbname=[redacted] <bundle>/main.postgres.dump",
		Redactions: []string{dsn},
	}, records)
	return err
}

func parseMySQLDSN(dsn string) (*mysqlcfg.Config, error) {
	parsed, err := mysqlcfg.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse MySQL DB_DSN: %w", err)
	}
	if parsed.DBName == "" {
		return nil, errors.New("MySQL DB_DSN must name a database")
	}
	if parsed.Net == "" {
		parsed.Net = "tcp"
	}
	if parsed.Addr == "" {
		parsed.Addr = "127.0.0.1:3306"
	}
	return parsed, nil
}

func mysqlArgs(cfg *mysqlcfg.Config) ([]string, []string, error) {
	args := []string{"--user=" + cfg.User}
	switch cfg.Net {
	case "tcp", "tcp4", "tcp6":
		host, port, err := net.SplitHostPort(cfg.Addr)
		if err != nil {
			return nil, nil, fmt.Errorf("parse MySQL address %q: %w", cfg.Addr, err)
		}
		args = append(args, "--host="+host, "--port="+port, "--protocol=TCP")
	case "unix":
		args = append(args, "--socket="+cfg.Addr, "--protocol=SOCKET")
	default:
		return nil, nil, fmt.Errorf("unsupported MySQL network %q", cfg.Net)
	}
	switch strings.ToLower(cfg.TLSConfig) {
	case "", "false", "preferred":
		// Native client default remains unchanged.
	case "true":
		args = append(args, "--ssl-mode=REQUIRED")
	case "skip-verify":
		args = append(args, "--ssl-mode=REQUIRED")
	default:
		return nil, nil, fmt.Errorf("native MySQL backup cannot resolve custom TLS config %q", cfg.TLSConfig)
	}
	env := []string{}
	if cfg.Passwd != "" {
		env = append(env, "MYSQL_PWD="+cfg.Passwd)
	}
	return args, env, nil
}

func captureMySQL(ctx context.Context, dsn, target string, runner CommandRunner, records *[]CommandRecord) (string, error) {
	cfg, err := parseMySQLDSN(dsn)
	if err != nil {
		return "", err
	}
	if err := requireVersion(ctx, runner, "mysqldump", []string{"--version"}, "mysqldump --version", "Ver 8.4.", records, cfg.Passwd, dsn); err != nil {
		return "", err
	}
	args, env, err := mysqlArgs(cfg)
	if err != nil {
		return "", err
	}
	args = append(args,
		"--single-transaction",
		"--quick",
		"--hex-blob",
		"--no-tablespaces",
		"--skip-comments",
		cfg.DBName,
	)
	_, err = runRecorded(ctx, runner, "capture-main", Command{
		Name:       "mysqldump",
		Args:       args,
		Env:        env,
		StdoutPath: target,
		Display:    "mysqldump --single-transaction --quick --hex-blob --no-tablespaces <database> > <bundle>/main.mysql.sql",
		Redactions: []string{cfg.Passwd, dsn},
	}, records)
	if err != nil {
		return "", err
	}
	info, err := requireRegular(target)
	if err != nil {
		return "", err
	}
	if info.Size() == 0 {
		return "", errors.New("mysqldump produced an empty artifact")
	}
	return "MySQL 8.4 logical dump captured; successful fresh-target import is the integrity proof", nil
}

func restoreMySQL(ctx context.Context, source, dsn string, runner CommandRunner, records *[]CommandRecord) error {
	cfg, err := parseMySQLDSN(dsn)
	if err != nil {
		return err
	}
	if err := requireVersion(ctx, runner, "mysql", []string{"--version"}, "mysql --version", "Ver 8.4.", records, cfg.Passwd, dsn); err != nil {
		return err
	}
	args, env, err := mysqlArgs(cfg)
	if err != nil {
		return err
	}
	args = append(args, "--binary-mode", cfg.DBName)
	_, err = runRecorded(ctx, runner, "restore-main", Command{
		Name:       "mysql",
		Args:       args,
		Env:        env,
		StdinPath:  source,
		Display:    "mysql --binary-mode <database> < <bundle>/main.mysql.sql",
		Redactions: []string{cfg.Passwd, dsn},
	}, records)
	return err
}

func ensureFreshServerDatabase(ctx context.Context, driver, dsn string) error {
	db, err := storage.NewDatabase(driver, dsn)
	if err != nil {
		return fmt.Errorf("open fresh restore target: %w", err)
	}
	defer closeGORM(db)
	version, err := databaseVersion(ctx, db, driver)
	if err != nil {
		return err
	}
	if err := validateEngineProfile(driver, version); err != nil {
		return err
	}
	query := ""
	switch driver {
	case "postgres":
		query = "SELECT COUNT(*) FROM information_schema.tables WHERE table_type='BASE TABLE' AND table_schema NOT IN ('pg_catalog','information_schema')"
	case "mysql":
		query = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()"
	default:
		return fmt.Errorf("fresh server database check is unsupported for %s", driver)
	}
	var count int64
	if err := db.WithContext(ctx).Raw(query).Scan(&count).Error; err != nil {
		return fmt.Errorf("inspect fresh restore target: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("restore target is not fresh: found %d user tables", count)
	}
	return nil
}

func parseMSSQLDSN(dsn string) (msdsn.Config, error) {
	parsed, err := msdsn.Parse(dsn)
	if err != nil {
		return msdsn.Config{}, fmt.Errorf("parse SQL Server DB_DSN: %w", err)
	}
	if parsed.Host == "" || parsed.Database == "" {
		return msdsn.Config{}, errors.New("SQL Server DB_DSN must name a host and database")
	}
	if parsed.Port == 0 {
		parsed.Port = 1433
	}
	return parsed, nil
}

func sqlcmdBase(cfg msdsn.Config) ([]string, []string) {
	server := cfg.Host + "," + strconv.FormatUint(cfg.Port, 10)
	args := []string{"-S", server, "-d", "master", "-b", "-r", "1"}
	if cfg.User != "" {
		args = append(args, "-U", cfg.User, "-P", cfg.Password)
	} else {
		args = append(args, "-E")
	}
	if cfg.TrustServerCertificate {
		args = append(args, "-C")
	}
	return args, []string{cfg.Password}
}

func captureMSSQL(ctx context.Context, dsn, target string, runner CommandRunner, records *[]CommandRecord) (string, error) {
	cfg, err := parseMSSQLDSN(dsn)
	if err != nil {
		return "", err
	}
	if err := requireVersion(ctx, runner, "sqlcmd", []string{"-?"}, "sqlcmd -?", "Version 18.", records, cfg.Password, dsn); err != nil {
		return "", err
	}
	base, redactions := sqlcmdBase(cfg)
	backupSQL := fmt.Sprintf("SET NOCOUNT ON; BACKUP DATABASE %s TO DISK = N'%s' WITH COPY_ONLY, CHECKSUM, INIT;", mssqlIdentifier(cfg.Database), mssqlLiteral(target))
	_, err = runRecorded(ctx, runner, "capture-main", Command{
		Name:       "sqlcmd",
		Args:       append(append([]string{}, base...), "-Q", backupSQL),
		Display:    "sqlcmd <server> -d master -Q BACKUP DATABASE <database> WITH COPY_ONLY,CHECKSUM,INIT",
		Redactions: append(redactions, dsn),
	}, records)
	if err != nil {
		return "", err
	}
	if _, err := requireRegular(target); err != nil {
		return "", fmt.Errorf("SQL Server backup artifact is not visible at %s; mount the absolute --out directory into SQL Server: %w", target, err)
	}
	verifySQL := fmt.Sprintf("SET NOCOUNT ON; RESTORE VERIFYONLY FROM DISK = N'%s' WITH CHECKSUM;", mssqlLiteral(target))
	_, err = runRecorded(ctx, runner, "integrity-main", Command{
		Name:       "sqlcmd",
		Args:       append(append([]string{}, base...), "-Q", verifySQL),
		Display:    "sqlcmd <server> -d master -Q RESTORE VERIFYONLY WITH CHECKSUM",
		Redactions: append(redactions, dsn),
	}, records)
	if err != nil {
		return "", err
	}
	return "SQL Server full COPY_ONLY backup verified by RESTORE VERIFYONLY WITH CHECKSUM", nil
}

type mssqlBackupFile struct {
	Logical string
	Kind    string
}

func restoreMSSQL(ctx context.Context, source, dsn string, runner CommandRunner, records *[]CommandRecord) error {
	cfg, err := parseMSSQLDSN(dsn)
	if err != nil {
		return err
	}
	if err := requireVersion(ctx, runner, "sqlcmd", []string{"-?"}, "sqlcmd -?", "Version 18.", records, cfg.Password, dsn); err != nil {
		return err
	}
	base, redactions := sqlcmdBase(cfg)
	checkSQL := fmt.Sprintf("SET NOCOUNT ON; SELECT CASE WHEN DB_ID(N'%s') IS NULL THEN 'missing' ELSE 'exists' END;", mssqlLiteral(cfg.Database))
	check, err := runRecorded(ctx, runner, "validate-fresh-target", Command{
		Name:       "sqlcmd",
		Args:       append(append([]string{}, base...), "-W", "-h", "-1", "-Q", checkSQL),
		Display:    "sqlcmd <server> -d master -Q validate fresh target",
		Redactions: append(redactions, dsn),
	}, records)
	if err != nil {
		return err
	}
	if !strings.Contains(check.Output, "missing") || strings.Contains(check.Output, "exists") {
		return fmt.Errorf("restore target is not fresh: SQL Server database %q already exists", cfg.Database)
	}
	fileListSQL := fmt.Sprintf("SET NOCOUNT ON; RESTORE FILELISTONLY FROM DISK = N'%s';", mssqlLiteral(source))
	fileList, err := runRecorded(ctx, runner, "inspect-backup-files", Command{
		Name:       "sqlcmd",
		Args:       append(append([]string{}, base...), "-W", "-h", "-1", "-s", "|", "-Q", fileListSQL),
		Display:    "sqlcmd <server> -d master -Q RESTORE FILELISTONLY",
		Redactions: append(redactions, dsn),
	}, records)
	if err != nil {
		return err
	}
	files, err := parseMSSQLFileList(fileList.Output)
	if err != nil {
		return err
	}
	pathsSQL := "SET NOCOUNT ON; SELECT CONVERT(nvarchar(4000),SERVERPROPERTY('InstanceDefaultDataPath')) + '|' + CONVERT(nvarchar(4000),SERVERPROPERTY('InstanceDefaultLogPath'));"
	paths, err := runRecorded(ctx, runner, "inspect-server-paths", Command{
		Name:       "sqlcmd",
		Args:       append(append([]string{}, base...), "-W", "-h", "-1", "-Q", pathsSQL),
		Display:    "sqlcmd <server> -d master -Q inspect default data paths",
		Redactions: append(redactions, dsn),
	}, records)
	if err != nil {
		return err
	}
	dataDir, logDir, err := parseMSSQLDefaultPaths(paths.Output)
	if err != nil {
		return err
	}
	moves := make([]string, 0, len(files))
	dataIndex, logIndex := 0, 0
	for _, file := range files {
		directory := dataDir
		extension := ".ndf"
		index := dataIndex
		if file.Kind == "L" {
			directory = logDir
			extension = ".ldf"
			index = logIndex
			logIndex++
		} else {
			if dataIndex == 0 {
				extension = ".mdf"
			}
			dataIndex++
		}
		physical := filepath.Join(directory, fmt.Sprintf("%s_%d%s", safeMSSQLFileName(cfg.Database), index, extension))
		moves = append(moves, fmt.Sprintf("MOVE N'%s' TO N'%s'", mssqlLiteral(file.Logical), mssqlLiteral(physical)))
	}
	restoreSQL := fmt.Sprintf("SET NOCOUNT ON; RESTORE DATABASE %s FROM DISK = N'%s' WITH CHECKSUM, RECOVERY, %s;", mssqlIdentifier(cfg.Database), mssqlLiteral(source), strings.Join(moves, ", "))
	_, err = runRecorded(ctx, runner, "restore-main", Command{
		Name:       "sqlcmd",
		Args:       append(append([]string{}, base...), "-Q", restoreSQL),
		Display:    "sqlcmd <server> -d master -Q RESTORE DATABASE <fresh-target> WITH MOVE,CHECKSUM,RECOVERY",
		Redactions: append(redactions, dsn),
	}, records)
	if err != nil {
		return err
	}
	checkDBSQL := fmt.Sprintf("SET NOCOUNT ON; DBCC CHECKDB (%s) WITH NO_INFOMSGS;", mssqlIdentifier(cfg.Database))
	_, err = runRecorded(ctx, runner, "integrity-restored-main", Command{
		Name:       "sqlcmd",
		Args:       append(append([]string{}, base...), "-Q", checkDBSQL),
		Display:    "sqlcmd <server> -d master -Q DBCC CHECKDB <fresh-target>",
		Redactions: append(redactions, dsn),
	}, records)
	return err
}

func parseMSSQLFileList(output string) ([]mssqlBackupFile, error) {
	var files []mssqlBackupFile
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			continue
		}
		logical := strings.TrimSpace(parts[0])
		kind := strings.TrimSpace(parts[2])
		if logical != "" && (kind == "D" || kind == "L") {
			files = append(files, mssqlBackupFile{Logical: logical, Kind: kind})
		}
	}
	if len(files) == 0 {
		return nil, errors.New("RESTORE FILELISTONLY returned no data or log files")
	}
	return files, nil
}

func parseMSSQLDefaultPaths(output string) (string, string, error) {
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) == 2 && strings.TrimSpace(parts[0]) != "" && strings.TrimSpace(parts[1]) != "" {
			return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
		}
	}
	return "", "", errors.New("SQL Server did not report default data and log paths")
}

func mssqlIdentifier(value string) string {
	return "[" + strings.ReplaceAll(value, "]", "]]") + "]"
}

func mssqlLiteral(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func safeMSSQLFileName(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "otelcontext_restore"
	}
	return builder.String()
}
