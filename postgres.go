package main

import (
	"fmt"
	"net/url"

	"github.com/rs/zerolog/log"
)

func exportPostgres(cfg SyncConfig, dataPath string) error {
	if cfg.SourceURI == "" {
		return fmt.Errorf("source URI is required for Postgres export")
	}

	args := []string{
		"--dbname", cfg.SourceURI,
		"--format=d",
		"--clean",
		"--if-exists",
		"--file", dataPath,
		"--jobs", cfg.Threads,
		"--verbose",
	}

	if err := runCmd("pg_dump", args...); err != nil {
		return fmt.Errorf("pg_dump failed: %w", err)
	}

	return nil
}

func transformPostgres(cfg SyncConfig, dataPath, transformedPath string) error {
	u, err := url.Parse(cfg.TransformURI)
	if err != nil {
		return fmt.Errorf("invalid transform URI: %w", err)
	}
	transformDBName := extractDBName(cfg.TransformURI)

	if transformDBName != "" {
		log.Info().Msgf("Ensuring clean state: Dropping and recreating Postgres database '%s'...\n", transformDBName)

		// To create/drop a database in Postgres, we must connect to an existing default database first
		baseURI := *u
		baseURI.Path = "/postgres"

		dropCmd := fmt.Sprintf("DROP DATABASE IF EXISTS \"%s\" WITH (FORCE);", transformDBName)
		createCmd := fmt.Sprintf("CREATE DATABASE \"%s\";", transformDBName)

		if err := runCmd("psql", "--dbname", baseURI.String(), "-c", dropCmd); err != nil {
			return fmt.Errorf("failed to clean (drop) transformation database: %w", err)
		}
		if err := runCmd("psql", "--dbname", baseURI.String(), "-c", createCmd); err != nil {
			return fmt.Errorf("failed to create transformation database: %w", err)
		}
	}

	log.Info().Msg("Importing raw data to local Postgres sidecar...")
	pgRestoreArgs := []string{
		"--dbname", cfg.TransformURI,
		"--format=d",
		"--jobs", cfg.Threads,
		"--clean",
		dataPath,
	}
	if err := runCmd("pg_restore", pgRestoreArgs...); err != nil {
		log.Info().Msgf("pg_restore completed (may have warnings): %v\n", err)
	}

	log.Info().Msg("Executing anonymisation scripts...")
	psqlArgs := []string{
		"--dbname", cfg.TransformURI,
		"--file", cfg.TransformScript,
	}
	if err := runCmd("psql", psqlArgs...); err != nil {
		return fmt.Errorf("sanitisation script failed: %w", err)
	}

	log.Info().Msg("Exporting sanitised data from local Postgres sidecar...")
	pgDumpArgs := []string{
		"--dbname", cfg.TransformURI,
		"--format=d",
		"--file", transformedPath,
		"--jobs", cfg.Threads,
		"--clean",
		"--if-exists",
	}
	if err := runCmd("pg_dump", pgDumpArgs...); err != nil {
		return fmt.Errorf("pg_dump export failed: %w", err)
	}

	return nil
}

func restorePostgres(cfg SyncConfig, dataPath string) error {
	u, err := url.Parse(cfg.DestURI)
	if err != nil {
		return fmt.Errorf("invalid dest URI: %w", err)
	}
	destDBName := extractDBName(cfg.DestURI)
	if destDBName == "" {
		return fmt.Errorf("Postgres restore requires a target database name in the DEST_URI")
	}

	tmpDBName := destDBName + "_tmp"
	oldDBName := destDBName + "_old"

	// Connect to the default 'postgres' database to issue admin commands
	adminURI := *u
	adminURI.Path = "/postgres"
	adminURIStr := adminURI.String()

	execAdmin := func(query string) error {
		return runCmd("psql", "--dbname", adminURIStr, "-c", query)
	}

	killConnsSQL := func(db string) string {
		return fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s' AND pid <> pg_backend_pid() AND usename <> 'rdsadmin';", db)
	}

	log.Info().Msgf("Preparing temporary restore database '%s'...", tmpDBName)

	// Safely terminate hanging connections to the tmp database, excluding AWS rdsadmin, then recreate it
	_ = execAdmin(killConnsSQL(tmpDBName))
	_ = execAdmin(fmt.Sprintf("DROP DATABASE IF EXISTS \"%s\";", tmpDBName))

	if err := execAdmin(fmt.Sprintf("CREATE DATABASE \"%s\";", tmpDBName)); err != nil {
		return fmt.Errorf("failed to create tmp database: %w", err)
	}

	// Apply temporary session-level performance tweaks to the new temporary database
	log.Info().Msg("Applying temporary performance tunings to the restore database...")
	tuningCmds := []string{
		fmt.Sprintf("ALTER DATABASE \"%s\" SET maintenance_work_mem = '2GB';", tmpDBName),
		fmt.Sprintf("ALTER DATABASE \"%s\" SET work_mem = '32MB';", tmpDBName),
		// Disabling synchronous commit gives a massive I/O speedup during restore
		// (comparable to turning off full_page_writes) without requiring a server reboot.
		fmt.Sprintf("ALTER DATABASE \"%s\" SET synchronous_commit = 'off';", tmpDBName),
	}
	for _, tCmd := range tuningCmds {
		if err := execAdmin(tCmd); err != nil {
			log.Warn().Err(err).Msgf("Failed to apply performance tuning (skipping): %s", tCmd)
		}
	}

	revertTunings := func() {
		log.Info().Msg("Reverting temporary performance tunings...")
		resetCmds := []string{
			fmt.Sprintf("ALTER DATABASE \"%s\" RESET maintenance_work_mem;", tmpDBName),
			fmt.Sprintf("ALTER DATABASE \"%s\" RESET work_mem;", tmpDBName),
			fmt.Sprintf("ALTER DATABASE \"%s\" RESET synchronous_commit;", tmpDBName),
		}
		for _, rCmd := range resetCmds {
			if err := execAdmin(rCmd); err != nil {
				log.Warn().Err(err).Msgf("Failed to revert performance tuning: %s", rCmd)
			}
		}
	}

	// Configure pg_restore to target the temporary database
	restoreURI := *u
	restoreURI.Path = "/" + tmpDBName

	log.Info().Msgf("Restoring Postgres data into temporary database '%s'...", tmpDBName)

	pgRestoreArgs := []string{
		"--dbname", restoreURI.String(),
		"--format=d",
		"--jobs", cfg.Threads,
		"--no-owner",      // Prevent restoring original production ownership
		"--no-privileges", // Prevent restoring original production permissions
		"--clean",
		"--if-exists", // Suppress harmless warnings about non-existent objects to ensure clean exit codes
		dataPath,
	}

	if err := runCmd("pg_restore", pgRestoreArgs...); err != nil {
		revertTunings()
		return fmt.Errorf("pg_restore encountered an error. Aborting swap to prevent corrupted database going live: %w", err)
	}

	// Reassign ownership securely if explicitly requested
	if cfg.DBOwner != "" {
		log.Info().Msgf("Setting database and object ownership to '%s'...", cfg.DBOwner)

		if err := execAdmin(fmt.Sprintf("ALTER DATABASE \"%s\" OWNER TO \"%s\";", tmpDBName, cfg.DBOwner)); err != nil {
			revertTunings()
			return fmt.Errorf("failed to alter database owner. Aborting swap: %w", err)
		}

		if err := runCmd("psql", "--dbname", restoreURI.String(), "-c", fmt.Sprintf("ALTER SCHEMA public OWNER TO \"%s\";", cfg.DBOwner)); err != nil {
			revertTunings()
			return fmt.Errorf("failed to alter public schema owner. Aborting swap: %w", err)
		}

		if err := runCmd("psql", "--dbname", restoreURI.String(), "-c", fmt.Sprintf("REASSIGN OWNED BY CURRENT_USER TO \"%s\";", cfg.DBOwner)); err != nil {
			revertTunings()
			return fmt.Errorf("failed to reassign object ownership. Aborting swap: %w", err)
		}
	}

	revertTunings()

	log.Info().Msgf("Restore successful. Swapping '%s' to '%s'...", tmpDBName, destDBName)

	// Prevent new connections to the live database (ignoring errors if it doesn't exist yet)
	_ = execAdmin(fmt.Sprintf("ALTER DATABASE \"%s\" WITH ALLOW_CONNECTIONS false;", destDBName))

	// Drop the previous _old database to make room
	_ = execAdmin(fmt.Sprintf("DROP DATABASE IF EXISTS \"%s\";", oldDBName))

	// Perform the atomic swap by terminating connections and renaming inside a single execution block
	swapScript := fmt.Sprintf(`
BEGIN;
%[1]s
ALTER DATABASE "%[2]s" RENAME TO "%[3]s";
%[4]s
ALTER DATABASE "%[5]s" RENAME TO "%[2]s";
COMMIT;
`, killConnsSQL(destDBName), destDBName, oldDBName, killConnsSQL(tmpDBName), tmpDBName)

	if err := execAdmin(swapScript); err != nil {
		log.Info().Msgf("Atomic swap failed (likely first run and '%s' doesn't exist). Attempting direct rename...", destDBName)

		renameTmpCmd := fmt.Sprintf(`
BEGIN;
%[1]s
ALTER DATABASE "%[2]s" RENAME TO "%[3]s";
COMMIT;
`, killConnsSQL(tmpDBName), tmpDBName, destDBName)

		if err := execAdmin(renameTmpCmd); err != nil {
			return fmt.Errorf("failed to swap temporary database into place: %w", err)
		}
	} else {
		// If the swap succeeded, re-enable connections on the old database so we don't leave it locked
		_ = execAdmin(fmt.Sprintf("ALTER DATABASE \"%s\" WITH ALLOW_CONNECTIONS true;", oldDBName))
	}

	log.Info().Msgf("Swap successful. Cleaning up old database '%s'...", oldDBName)
	if err := execAdmin(fmt.Sprintf("DROP DATABASE IF EXISTS \"%s\";", oldDBName)); err != nil {
		log.Warn().Err(err).Msgf("Failed to drop old database '%s' (you may need to clean it up manually)", oldDBName)
	}

	log.Info().Msg("Postgres database swap complete.")
	return nil
}
