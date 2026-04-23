package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/rs/zerolog/log"
)

func exportMysql(cfg SyncConfig, dataPath string) error {
	if cfg.SourceURI == "" {
		return fmt.Errorf("source URI is required for MySQL export")
	}

	u, err := url.Parse(cfg.SourceURI)
	if err != nil {
		return fmt.Errorf("invalid source URI: %w", err)
	}

	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "3306"
	}
	user := u.User.Username()
	password, _ := u.User.Password()

	if cfg.DBName == "" {
		return fmt.Errorf("MySQL export requires a target database name in the DB_NAME env var or SOURCE_URI path")
	}

	args := []string{
		"--host", host,
		"--port", port,
		"--user", user,
		"--password", password,
		"--database", cfg.DBName,
		"--outputdir", dataPath,
		"--threads", cfg.Threads,
		"--clear",
		"--build-empty-files",
		"--sync-thread-lock-mode=SAFE_NO_LOCK",
		"--trx-tables",
		"--verbose=3",
	}

	if err = runCmd("mydumper", args...); err != nil {
		return fmt.Errorf("mydumper failed: %w", err)
	}

	return nil
}

func transformMysql(cfg SyncConfig, dataPath, transformedPath string) error {
	u, err := url.Parse(cfg.TransformURI)
	if err != nil {
		return fmt.Errorf("invalid transform URI: %w", err)
	}

	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "3306"
	}
	user := u.User.Username()
	password, _ := u.User.Password()
	transformDBName := extractDBName(cfg.TransformURI)

	if transformDBName != "" {
		log.Info().Msgf("Ensuring clean state: Dropping and recreating database '%s'...\n", transformDBName)
		cleanupArgs := []string{
			"--host", host,
			"--port", port,
			"--user", user,
			fmt.Sprintf("--password=%s", password),
			"-e", fmt.Sprintf("DROP DATABASE IF EXISTS `%s`; CREATE DATABASE `%s`;", transformDBName, transformDBName),
		}
		if err := runCmd("mysql", cleanupArgs...); err != nil {
			return fmt.Errorf("failed to clean sidecar database: %w", err)
		}
	}

	log.Info().Msg("Importing raw data to local MySQL sidecar...")
	myloaderArgs := []string{
		"--host", host,
		"--port", port,
		"--user", user,
		"--password", password,
		"--database", transformDBName,
		"--directory", dataPath,
		"--threads", cfg.Threads,
		"--verbose=3",
	}
	if err := runCmd("myloader", myloaderArgs...); err != nil {
		return fmt.Errorf("myloader import failed: %w", err)
	}

	log.Info().Msg("Executing anonymisation scripts...")
	mysqlArgs := []string{
		"--host", host,
		"--port", port,
		"--user", user,
		fmt.Sprintf("--password=%s", password),
		"--database", transformDBName,
		"-e", fmt.Sprintf("source %s", cfg.TransformScript),
	}
	if err := runCmd("mysql", mysqlArgs...); err != nil {
		return fmt.Errorf("sanitisation script failed: %w", err)
	}

	log.Info().Msg("Exporting sanitised data from local MySQL sidecar...")
	mydumperArgs := []string{
		"--host", host,
		"--port", port,
		"--user", user,
		"--password", password,
		"--database", transformDBName,
		"--outputdir", transformedPath,
		"--threads", cfg.Threads,
		"--clear",
		"--build-empty-files",
		"--sync-thread-lock-mode=SAFE_NO_LOCK",
		"--trx-tables",
	}
	if err := runCmd("mydumper", mydumperArgs...); err != nil {
		return fmt.Errorf("mydumper export failed: %w", err)
	}

	return nil
}

func restoreMysql(cfg SyncConfig, dataPath string) error {
	u, err := url.Parse(cfg.DestURI)
	if err != nil {
		return fmt.Errorf("invalid dest URI: %w", err)
	}

	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "3306"
	}
	user := u.User.Username()
	password, _ := u.User.Password()

	destDBName := extractDBName(cfg.DestURI)
	if destDBName == "" {
		return fmt.Errorf("MySQL restore requires a target database name in the DEST_URI")
	}

	tmpDBName := destDBName + "_tmp"
	oldDBName := destDBName + "_old"

	execMysql := func(query string) error {
		return runCmd("mysql", "--host", host, "--port", port, "--user", user, fmt.Sprintf("--password=%s", password), "-e", query)
	}

	fetchTables := func(db string, tableType string) ([]string, error) {
		out, err := runCmdWithOutput("mysql", "--host", host, "--port", port, "--user", user, fmt.Sprintf("--password=%s", password), "-N", "-B", "-e", fmt.Sprintf("SELECT table_name FROM information_schema.tables WHERE table_schema = '%s' AND table_type = '%s';", db, tableType))
		if err != nil {
			return nil, err
		}
		return strings.Fields(string(out)), nil
	}

	log.Info().Msgf("Preparing temporary restore database '%s'...", tmpDBName)

	_ = execMysql(fmt.Sprintf("DROP DATABASE IF EXISTS `%s`;", tmpDBName))
	if err := execMysql(fmt.Sprintf("CREATE DATABASE `%s`;", tmpDBName)); err != nil {
		return fmt.Errorf("failed to create tmp database: %w", err)
	}

	log.Info().Msgf("Restoring MySQL data into temporary database '%s'...", tmpDBName)

	myloaderArgs := []string{
		"--host", host,
		"--port", port,
		"--user", user,
		"--password", password,
		"--database", tmpDBName, // Safely force restoration into the tmp database
		"--directory", dataPath,
		"--threads", cfg.Threads,
		"--verbose=3",
	}
	if err := runCmd("myloader", myloaderArgs...); err != nil {
		return fmt.Errorf("myloader restore failed. Aborting swap to prevent corrupted database going live: %w", err)
	}

	log.Info().Msgf("Restore successful. Swapping '%s' to '%s'...", tmpDBName, destDBName)

	// Ensure live database exists (crucial for the first-ever run)
	_ = execMysql(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`;", destDBName))

	// Ensure old database exists and is empty for the swap out
	_ = execMysql(fmt.Sprintf("DROP DATABASE IF EXISTS `%s`;", oldDBName))
	if err := execMysql(fmt.Sprintf("CREATE DATABASE `%s`;", oldDBName)); err != nil {
		return fmt.Errorf("failed to create old database for swap: %w", err)
	}

	tmpTables, err := fetchTables(tmpDBName, "BASE TABLE")
	if err != nil {
		return fmt.Errorf("failed to fetch base tables from tmp database: %w", err)
	}

	liveTablesList, err := fetchTables(destDBName, "BASE TABLE")
	if err != nil {
		return fmt.Errorf("failed to fetch base tables from live database: %w", err)
	}

	liveTables := make(map[string]bool)
	for _, t := range liveTablesList {
		liveTables[t] = true
	}

	var renameParts []string
	for _, t := range tmpTables {
		if liveTables[t] {
			// Atomically move the live table safely out of the way
			renameParts = append(renameParts, fmt.Sprintf("`%s`.`%s` TO `%s`.`%s`", destDBName, t, oldDBName, t))
		}
		// Atomically move the freshly restored tmp table into the live position
		renameParts = append(renameParts, fmt.Sprintf("`%s`.`%s` TO `%s`.`%s`", tmpDBName, t, destDBName, t))
		delete(liveTables, t)
	}

	// Move any leftover live tables (e.g. ones that don't exist in the new backup) to the old DB
	for t := range liveTables {
		renameParts = append(renameParts, fmt.Sprintf("`%s`.`%s` TO `%s`.`%s`", destDBName, t, oldDBName, t))
	}

	if len(renameParts) > 0 {
		renameQuery := "RENAME TABLE " + strings.Join(renameParts, ", ") + ";"
		if err := execMysql(renameQuery); err != nil {
			return fmt.Errorf("atomic MySQL table swap failed: %w", err)
		}
	} else {
		log.Info().Msg("No base tables found to swap.")
	}

	// Transfer Views (MySQL restricts moving views across databases using RENAME TABLE)
	viewList, err := fetchTables(tmpDBName, "VIEW")
	if err == nil && len(viewList) > 0 {
		log.Info().Msg("Transferring views from tmp to live database...")
		dumpArgs := []string{"--host", host, "--port", port, "--user", user, fmt.Sprintf("--password=%s", password), "--no-data", tmpDBName}
		dumpArgs = append(dumpArgs, viewList...)

		restoreArgs := []string{"--host", host, "--port", port, "--user", user, fmt.Sprintf("--password=%s", password), destDBName}

		if err := runPipe("mysqldump", dumpArgs, "mysql", restoreArgs); err != nil {
			log.Warn().Err(err).Msg("Failed to transfer views.")
		}
	}

	log.Info().Msgf("Swap successful. Cleaning up old and tmp databases...")
	_ = execMysql(fmt.Sprintf("DROP DATABASE IF EXISTS `%s`;", oldDBName))
	_ = execMysql(fmt.Sprintf("DROP DATABASE IF EXISTS `%s`;", tmpDBName))

	log.Info().Msg("MySQL database swap complete.")
	return nil
}
