package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"
	_ "go.uber.org/automaxprocs"
)

// SyncConfig holds the configuration for the database sync pipeline
type SyncConfig struct {
	AppName         string
	DBType          string
	DBName          string
	DBOwner         string
	DocDBSourceDB   string
	SourceURI       string
	DestURI         string
	TransformURI    string
	TransformScript string
	S3Bucket        string
	S3Path          string
	S3Region        string
	PushgatewayURL  string
	Threads         string
	DryRun          bool
}

func init() {
	// Configure zerolog to match the rest of the project
	if os.Getenv("GOVUK_ENVIRONMENT") == "" {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	}
}

// exportData routes to the engine-specific export logic
func exportData(cfg SyncConfig, dataPath string) error {
	if cfg.DBName == "" {
		cfg.DBName = extractDBName(cfg.SourceURI)
	}

	if cfg.DryRun {
		log.Info().Msgf("[DRY RUN] Would execute %s database export to: %s", cfg.DBType, dataPath)
		return nil
	}

	switch cfg.DBType {
	case "mysql":
		return exportMysql(cfg, dataPath)
	case "postgres":
		return exportPostgres(cfg, dataPath)
	case "documentdb":
		return exportDocumentDB(cfg, dataPath)
	default:
		return fmt.Errorf("export not implemented for db type: %s", cfg.DBType)
	}
}

// transformData routes to the engine-specific transform logic
func transformData(cfg SyncConfig, dataPath string) (string, error) {
	if cfg.DBName == "" {
		cfg.DBName = extractDBName(cfg.SourceURI)
	}

	transformedPath := dataPath + "_transformed"

	if cfg.TransformURI == "" {
		return "", fmt.Errorf("transform URI is required for the transformation stage")
	}

	// Clean out any lingering files from previous interrupted runs before we start
	if _, err := os.Stat(transformedPath); !os.IsNotExist(err) {
		log.Info().Msg("Existing transformed directory found, wiping it...")
		if err := os.RemoveAll(transformedPath); err != nil {
			return "", fmt.Errorf("failed to remove transformed dump directory: %w", err)
		}
	}

	// Create required output directories ahead of execution
	if err := os.MkdirAll(transformedPath, 0755); err != nil {
		return "", fmt.Errorf("failed to create transform directory: %w", err)
	}

	if cfg.DryRun {
		log.Info().Msgf("[DRY RUN] Would import %s, run %s, and export to %s", dataPath, cfg.TransformScript, transformedPath)
		return transformedPath, nil
	}

	var err error
	switch cfg.DBType {
	case "mysql":
		err = transformMysql(cfg, dataPath, transformedPath)
	case "postgres":
		err = transformPostgres(cfg, dataPath, transformedPath)
	case "documentdb":
		err = transformDocumentDB(cfg, dataPath, transformedPath)
	default:
		return "", fmt.Errorf("transform not implemented for db type: %s", cfg.DBType)
	}
	return transformedPath, err
}

// restoreData routes to the engine-specific restore logic
func restoreData(cfg SyncConfig, dataPath string) error {
	if cfg.DBName == "" {
		cfg.DBName = extractDBName(cfg.DestURI)
	}

	if cfg.DryRun {
		log.Info().Msgf("[DRY RUN] Would restore data from %s to target database", dataPath)
		return nil
	}

	switch cfg.DBType {
	case "mysql":
		return restoreMysql(cfg, dataPath)
	case "postgres":
		return restorePostgres(cfg, dataPath)
	case "documentdb":
		return restoreDocumentDB(cfg, dataPath)
	default:
		return fmt.Errorf("restore not implemented for db type: %s", cfg.DBType)
	}
}

// setupConfig parses common flags and determines the base data path
func setupConfig(cmd *cli.Command) (SyncConfig, string) {
	threadCount := cmd.String("threads")
	if threadCount == "0" || threadCount == "auto" {
		cores := runtime.NumCPU()
		threadCount = strconv.Itoa(cores)
		log.Info().Msgf("Auto-detected CPU cores. Setting thread count to %s.", threadCount)
	}

	dbType := cmd.String("type")
	var dataPath string
	switch dbType {
	case "mysql":
		dataPath = "/tmp/mysql_dump"
	case "postgres":
		dataPath = "/tmp/pg_dump_dir"
	case "documentdb":
		dataPath = "/tmp/mongo_dump"
	default:
		dataPath = "/tmp/dump"
	}

	cfg := SyncConfig{
		AppName:         cmd.String("app-name"),
		DBType:          dbType,
		DBName:          cmd.String("db-name"),
		DBOwner:         cmd.String("db-owner"),
		DocDBSourceDB:   cmd.String("docdb-source-db"),
		SourceURI:       injectPassword(cmd.String("source"), cmd.String("source-password")),
		DestURI:         injectPassword(cmd.String("dest"), cmd.String("dest-password")),
		TransformURI:    injectPassword(cmd.String("transform-uri"), cmd.String("transform-password")),
		TransformScript: cmd.String("transform-script"),
		S3Bucket:        cmd.String("s3-bucket"),
		S3Path:          cmd.String("s3-path"),
		S3Region:        cmd.String("s3-region"),
		PushgatewayURL:  cmd.String("pushgateway-url"),
		Threads:         threadCount,
		DryRun:          cmd.Bool("dry-run"),
	}
	return cfg, dataPath
}

// getCommonFlags returns a fresh slice of common CLI flags.
// By returning a new slice with new pointers on every call, we safely de-duplicate
// the definitions without triggering urfave/cli state-sharing bugs across commands.
func getCommonFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:     "type",
			Usage:    "Database type (postgres, mysql, documentdb)",
			Sources:  cli.EnvVars("DB_TYPE"),
			Required: true,
		},
		&cli.StringFlag{
			Name:    "app-name",
			Usage:   "Name of the application (e.g. release)",
			Sources: cli.EnvVars("APP_NAME"),
		},
		&cli.StringFlag{
			Name:    "db-name",
			Usage:   "Name of the database",
			Sources: cli.EnvVars("DB_NAME"),
		},
		&cli.StringFlag{
			Name:    "db-owner",
			Usage:   "Owner role of the database (useful for Postgres restores)",
			Sources: cli.EnvVars("DB_OWNER"),
		},
		&cli.StringFlag{
			Name:     "s3-bucket",
			Usage:    "S3 bucket to upload/download the snapshot",
			Sources:  cli.EnvVars("S3_BUCKET"),
			Required: true,
		},
		&cli.StringFlag{
			Name:    "s3-path",
			Usage:   "Custom S3 object prefix",
			Sources: cli.EnvVars("S3_PATH"),
		},
		&cli.StringFlag{
			Name:    "s3-region",
			Usage:   "AWS region for the S3 bucket",
			Value:   "eu-west-1",
			Sources: cli.EnvVars("S3_REGION", "AWS_REGION"),
		},
		&cli.StringFlag{
			Name:    "pushgateway-url",
			Usage:   "Prometheus Pushgateway URL for metrics",
			Sources: cli.EnvVars("PUSHGATEWAY_URL"),
		},
		&cli.StringFlag{
			Name:    "threads",
			Value:   "0",
			Usage:   "Number of concurrent threads. Set to 0 to auto-detect.",
			Sources: cli.EnvVars("THREADS"),
		},
		&cli.BoolFlag{
			Name:    "dry-run",
			Usage:   "Simulate the pipeline without executing any shell commands or database operations",
			Sources: cli.EnvVars("DRY_RUN"),
		},
	}
}

// NewApp constructs the CLI application and wraps it so it can be instantiated in tests
func NewApp() *cli.Command {
	backupFlags := append(getCommonFlags(),
		&cli.StringFlag{
			Name:     "source",
			Usage:    "Source DB URI",
			Sources:  cli.EnvVars("SOURCE_URI"),
			Required: true,
		},
		&cli.StringFlag{
			Name:    "source-password",
			Usage:   "Password for the source database (overrides any password in the URI)",
			Sources: cli.EnvVars("SOURCE_PASSWORD"),
		},
		&cli.StringFlag{
			Name:    "transform-uri",
			Usage:   "Local DB URI for data transformation (e.g. sidecar container)",
			Sources: cli.EnvVars("TRANSFORM_URI"),
		},
		&cli.StringFlag{
			Name:    "transform-password",
			Usage:   "Password for the transform database",
			Sources: cli.EnvVars("TRANSFORM_PASSWORD"),
		},
		&cli.StringFlag{
			Name:    "transform-script",
			Usage:   "Path to the SQL/JS script for data anonymisation",
			Sources: cli.EnvVars("TRANSFORM_SCRIPT"),
		},
	)

	restoreFlags := append(getCommonFlags(),
		&cli.StringFlag{
			Name:     "dest",
			Usage:    "Destination DB URI",
			Sources:  cli.EnvVars("DEST_URI"),
			Required: true,
		},
		&cli.StringFlag{
			Name:    "dest-password",
			Usage:   "Password for the destination database (overrides any password in the URI)",
			Sources: cli.EnvVars("DEST_PASSWORD"),
		},
		&cli.StringFlag{
			Name:    "docdb-source-db",
			Usage:   "Original source database name (Required for DocumentDB namespace mapping)",
			Sources: cli.EnvVars("DOCDB_SOURCE_DB"),
		},
	)

	return &cli.Command{
		Name:  "db-sync",
		Usage: "GOV.UK Database Sync and Transform Pipeline",
		Commands: []*cli.Command{
			{
				Name:  "backup",
				Usage: "Export from source DB, optionally transform the data, and upload to S3",
				Flags: backupFlags,
				Action: func(ctx context.Context, cmd *cli.Command) error {
					cfg, dataPath := setupConfig(cmd)
					startTime := time.Now()
					var err error

					defer func(basePath string) {
						durationSecs := time.Since(startTime).Seconds()
						pushMetrics(cfg, "backup", durationSecs, err == nil)

						log.Info().Msg("Cleaning up temporary workspace...")
						os.RemoveAll(basePath)
						os.RemoveAll(basePath + "_transformed")
						os.Remove(basePath + ".tar.zst")
					}(dataPath)

					if err = exportData(cfg, dataPath); err != nil {
						return fmt.Errorf("export phase failed: %w", err)
					}

					rawSize, _ := getDirSize(dataPath)
					log.Info().Msgf("Exported raw data size: %s", humanize.Bytes(uint64(rawSize)))

					if cfg.TransformScript != "" {
						if _, err := os.Stat(cfg.TransformScript); os.IsNotExist(err) && cfg.DryRun {
							log.Info().Msgf("Transform script '%s' not found. Skipping Transform Phase as Dry-Run is enabled...", cfg.TransformScript)
						} else {
							if dataPath, err = transformData(cfg, dataPath); err != nil {
								return fmt.Errorf("transform phase failed: %w", err)
							}
							transSize, _ := getDirSize(dataPath)
							log.Info().Msgf("Transformed data size: %s", humanize.Bytes(uint64(transSize)))
						}
					} else {
						log.Info().Msg("No transform script provided (--transform-script). Skipping Transform Phase...")
					}

					var archiveSize int64
					archiveSize, err = uploadToS3(cfg, dataPath)
					if err != nil {
						return fmt.Errorf("s3 upload phase failed: %w", err)
					}

					finishTime := time.Now()
					log.Info().Msgf("Backup pipeline completed successfully in %s. Final archive size: %s", formatDuration(finishTime.Sub(startTime)), humanize.Bytes(uint64(archiveSize)))
					return nil
				},
			},
			{
				Name:  "restore",
				Usage: "Download a snapshot from S3 and import it to the destination DB",
				Flags: restoreFlags,
				Action: func(ctx context.Context, cmd *cli.Command) error {
					cfg, _ := setupConfig(cmd)
					startTime := time.Now()
					var err error

					extractPath := fmt.Sprintf("/tmp/%s_%s_downloaded", cfg.DBType, cfg.DBName)

					defer func() {
						durationSecs := time.Since(startTime).Seconds()
						pushMetrics(cfg, "restore", durationSecs, err == nil)

						log.Info().Msg("Cleaning up temporary workspace...")
						os.RemoveAll(extractPath)
					}()

					if os.Getenv("GOVUK_ENVIRONMENT") == "production" && !cfg.DryRun {
						log.Warn().Msg("=======================================================================")
						log.Warn().Msg("WARNING: YOU ARE ABOUT TO OVERWRITE A PRODUCTION DATABASE")
						log.Warn().Msg("=======================================================================")
					}

					var downloadedPath string
					downloadedPath, _, err = downloadFromS3(cfg)
					if err != nil {
						return fmt.Errorf("s3 download phase failed: %w", err)
					}

					if err = restoreData(cfg, downloadedPath); err != nil {
						return fmt.Errorf("restore phase failed: %w", err)
					}

					finishTime := time.Now()
					restoredSize, _ := getDirSize(downloadedPath)
					log.Info().Msgf("Restore pipeline completed successfully in %s. Data size processed: %s", formatDuration(finishTime.Sub(startTime)), humanize.Bytes(uint64(restoredSize)))
					return nil
				},
			},
		},
	}
}

func main() {
	if err := NewApp().Run(context.Background(), os.Args); err != nil {
		log.Fatal().Err(err).Msg("Pipeline execution failed")
	}
}
