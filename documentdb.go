package main

import (
	"fmt"
	"net/url"
	"os"

	"github.com/rs/zerolog/log"
)

func exportDocumentDB(cfg SyncConfig, dataPath string) error {
	if cfg.SourceURI == "" {
		return fmt.Errorf("source URI is required for DocumentDB export")
	}

	// Ensure the dump directory exists before mongodump attempts to write to it
	if err := os.MkdirAll(dataPath, 0755); err != nil {
		return fmt.Errorf("failed to create export directory: %w", err)
	}

	args := []string{
		"--uri", cfg.SourceURI,
		fmt.Sprintf("--out=%s", dataPath),
		"--numParallelCollections", cfg.Threads,
	}

	if err := runCmd("mongodump", args...); err != nil {
		return fmt.Errorf("mongodump failed: %w", err)
	}

	return nil
}

func transformDocumentDB(cfg SyncConfig, dataPath, transformedPath string) error {
	u, err := url.Parse(cfg.TransformURI)
	if err != nil {
		return fmt.Errorf("invalid transform URI: %w", err)
	}

	// Force sidecar to use the original Source DB name instead of the Transform DB name.
	originalDBName := cfg.DBName
	if originalDBName == "" {
		return fmt.Errorf("DocumentDB transform requires a target database name in the DB_NAME env var or SOURCE_URI path")
	}
	u.Path = "/" + originalDBName
	adjustedTransformURI := u.String()

	log.Info().Msgf("Ensuring clean state: Dropping DocumentDB database '%s'...\n", originalDBName)
	dropArgs := []string{
		adjustedTransformURI,
		"--eval", fmt.Sprintf("db.getSiblingDB('%s').dropDatabase()", originalDBName),
	}
	if err := runCmd("mongo", dropArgs...); err != nil {
		return fmt.Errorf("failed to drop sidecar database: %w", err)
	}

	// Restore raw payload into the Transform DB
	log.Info().Msg("Importing raw data to local DocumentDB sidecar...")

	restoreU, _ := url.Parse(adjustedTransformURI)
	restoreU.Path = "/"
	restoreURI := restoreU.String()

	mongoRestoreArgs := []string{
		"--uri", restoreURI,
		"--drop",
		fmt.Sprintf("--dir=%s", dataPath),
		// No nsFrom/nsTo needed here because the archive contains originalDBName,
		// and we are simply restoring it into originalDBName on the sidecar
		fmt.Sprintf("--nsInclude=%s.*", originalDBName),
		"--numParallelCollections", cfg.Threads,
		"--numInsertionWorkersPerCollection", cfg.Threads,
	}
	if err := runCmd("mongorestore", mongoRestoreArgs...); err != nil {
		return fmt.Errorf("mongorestore failed: %w", err)
	}

	// Run the sanitisation JS file via the mongo shell
	log.Info().Msg("Executing anonymisation scripts...")
	if err := runCmd("mongo", adjustedTransformURI, cfg.TransformScript); err != nil {
		return fmt.Errorf("sanitisation script failed: %w", err)
	}

	log.Info().Msg("Exporting sanitised data from local DocumentDB sidecar...")
	mongoDumpArgs := []string{
		"--uri", adjustedTransformURI,
		fmt.Sprintf("--out=%s", transformedPath),
		"--numParallelCollections", cfg.Threads,
	}
	if err := runCmd("mongodump", mongoDumpArgs...); err != nil {
		return fmt.Errorf("mongodump export failed: %w", err)
	}

	return nil
}

func restoreDocumentDB(cfg SyncConfig, dataPath string) error {
	u, err := url.Parse(cfg.DestURI)
	if err != nil {
		return fmt.Errorf("invalid dest URI: %w", err)
	}
	destDBName := extractDBName(cfg.DestURI)
	if destDBName == "" {
		return fmt.Errorf("DocumentDB restore requires a target database name in the DEST_URI")
	}

	if cfg.DocDBSourceDB == "" {
		return fmt.Errorf("CRITICAL: DocumentDB restore requires --docdb-source-db to map the original namespace to the destination")
	}

	u.Path = "/"
	restoreURI := u.String()

	mongoRestoreArgs := []string{
		"--uri", restoreURI,
		"--drop",
		fmt.Sprintf("--dir=%s", dataPath),
		fmt.Sprintf("--nsInclude=%s.*", cfg.DocDBSourceDB),
		fmt.Sprintf("--nsFrom=%s.*", cfg.DocDBSourceDB),
		fmt.Sprintf("--nsTo=%s.*", destDBName),
		"--numParallelCollections", cfg.Threads,
		"--numInsertionWorkersPerCollection", cfg.Threads,
	}

	if err := runCmd("mongorestore", mongoRestoreArgs...); err != nil {
		return fmt.Errorf("mongorestore failed: %w", err)
	}

	return nil
}
