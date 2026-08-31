package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/RandomCodeSpace/otelcontext/internal/backup"
	"github.com/RandomCodeSpace/otelcontext/internal/config"
)

const backupUsage = "usage: otelcontext backup <create --out ABSOLUTE_DIRECTORY|restore --bundle ABSOLUTE_DIRECTORY>"

func maybeRunBackupCommand(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 || args[0] != "backup" {
		return false, 0
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, backupUsage)
		return true, 2
	}
	command := args[1]
	flags := flag.NewFlagSet("backup "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var path *string
	switch command {
	case "create":
		path = flags.String("out", "", "absolute parent directory for the published bundle")
	case "restore":
		path = flags.String("bundle", "", "absolute completed bundle directory")
	default:
		fmt.Fprintln(stderr, backupUsage)
		return true, 2
	}
	if err := flags.Parse(args[2:]); err != nil || *path == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, backupUsage)
		return true, 2
	}
	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(stderr, "backup: load configuration: %v\n", err)
		return true, 1
	}
	candidate, err := backup.CurrentCandidate(Version)
	if err != nil {
		fmt.Fprintf(stderr, "backup: identify candidate: %v\n", err)
		return true, 1
	}
	backupCfg := backupConfig(cfg)
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	switch command {
	case "create":
		report, err := backup.Create(context.Background(), backupCfg, backup.CreateOptions{
			OutputDirectory: *path,
			Candidate:       candidate,
		})
		if err != nil {
			fmt.Fprintf(stderr, "backup create: %v\n", err)
			return true, 1
		}
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(stderr, "backup create: write report: %v\n", err)
			return true, 1
		}
	case "restore":
		report, err := backup.Restore(context.Background(), backupCfg, backup.RestoreOptions{
			BundleDirectory: *path,
			Candidate:       candidate,
		})
		if err != nil {
			fmt.Fprintf(stderr, "backup restore: %v\n", err)
			return true, 1
		}
		readySeconds, err := verifyRestoredCandidate(context.Background(), cfg)
		if err != nil {
			fmt.Fprintf(stderr, "backup restore: candidate readiness proof: %v\n", err)
			return true, 1
		}
		report.ReadySeconds = &readySeconds
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(stderr, "backup restore: write report: %v\n", err)
			return true, 1
		}
	}
	return true, 0
}

func backupConfig(cfg *config.Config) backup.Config {
	return backup.Config{
		DBDriver:               cfg.DBDriver,
		DBDSN:                  cfg.DBDSN,
		DBPostgresPartitioning: cfg.DBPostgresPartitioning,
		AggregateMode:          cfg.AggregateMode,
		AggregateDBPath:        cfg.AggregateDBPath,
		DLQPath:                cfg.DLQPath,
		DataDiskPath:           cfg.DataDiskPath,
		TLSCertFile:            cfg.TLSCertFile,
		TLSKeyFile:             cfg.TLSKeyFile,
		TLSAutoSelfSigned:      cfg.TLSAutoSelfsigned,
		TLSCacheDir:            cfg.TLSCacheDir,
	}
}
