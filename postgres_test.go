package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExportData_PostgresCleanup(t *testing.T) {
	_, executedCommands, teardown := setupTest()
	defer teardown()

	cfg := SyncConfig{
		DBType:    "postgres",
		SourceURI: "postgres://user:pass@localhost:5432/testdb",
		Threads:   "4",
	}

	dummyPath := "/tmp/test_pg_dump_dir"
	os.MkdirAll(dummyPath, 0755)
	defer os.RemoveAll(dummyPath)

	err := exportData(cfg, dummyPath)
	require.NoError(t, err)

	require.NotEmpty(t, *executedCommands)
	assert.Contains(t, (*executedCommands)[0], "pg_dump")
	assert.Contains(t, (*executedCommands)[0], "--clean")
}

func TestTransformData_PostgresTransactionSplit(t *testing.T) {
	_, executedCommands, teardown := setupTest()
	defer teardown()

	tempScript, _ := os.CreateTemp("", "transform-*.sql")
	defer os.Remove(tempScript.Name())

	cfg := SyncConfig{
		DBType:          "postgres",
		TransformURI:    "postgres://user:pass@localhost:5432/transformdb",
		TransformScript: tempScript.Name(),
		Threads:         "4",
	}

	path, err := transformData(cfg, "/tmp/test_dump")
	require.NoError(t, err)
	assert.Equal(t, "/tmp/test_dump_transformed", path)

	foundDrop := false
	foundCreate := false
	for _, cmd := range *executedCommands {
		if strings.Contains(cmd, "DROP DATABASE IF EXISTS \"transformdb\" WITH (FORCE);") {
			foundDrop = true
		}
		if strings.Contains(cmd, "CREATE DATABASE \"transformdb\";") {
			foundCreate = true
		}
	}
	assert.True(t, foundDrop, "expected separate DROP psql command")
	assert.True(t, foundCreate, "expected separate CREATE psql command")
}

func TestRestoreData_Postgres(t *testing.T) {
	_, executedCommands, teardown := setupTest()
	defer teardown()

	cfg := SyncConfig{
		DBType:  "postgres",
		DestURI: "postgres://user:pass@localhost:5432/destdb",
		Threads: "4",
		DBOwner: "app_user",
	}

	err := restoreData(cfg, "/tmp/dummy_restore_dir")
	require.NoError(t, err)

	require.NotEmpty(t, *executedCommands)

	// Convert command list to a single string for easier sequential assertion tracking
	cmds := strings.Join(*executedCommands, "\n")

	// Prepare Temporary Database
	assert.Contains(t, cmds, "DROP DATABASE IF EXISTS \"destdb_tmp\"")
	assert.Contains(t, cmds, "CREATE DATABASE \"destdb_tmp\"")

	// Apply Performance Tunings
	assert.Contains(t, cmds, "ALTER DATABASE \"destdb_tmp\" SET maintenance_work_mem = '2GB'")
	assert.Contains(t, cmds, "ALTER DATABASE \"destdb_tmp\" SET synchronous_commit = 'off'")

	// Restore Data safely
	assert.Contains(t, cmds, "pg_restore --dbname postgres://user:pass@localhost:5432/destdb_tmp")
	assert.Contains(t, cmds, "--no-owner")

	// Reassign Ownership safely
	assert.Contains(t, cmds, "ALTER DATABASE \"destdb_tmp\" OWNER TO \"app_user\"")
	assert.Contains(t, cmds, "ALTER SCHEMA public OWNER TO \"app_user\"")
	assert.Contains(t, cmds, "REASSIGN OWNED BY CURRENT_USER TO \"app_user\"")

	// Revert Tunings
	assert.Contains(t, cmds, "ALTER DATABASE \"destdb_tmp\" RESET synchronous_commit")

	// Disable connections and Swap Databases (Atomic Block)
	assert.Contains(t, cmds, "ALTER DATABASE \"destdb\" WITH ALLOW_CONNECTIONS false")
	assert.Contains(t, cmds, "ALTER DATABASE \"destdb\" RENAME TO \"destdb_old\"")
	assert.Contains(t, cmds, "ALTER DATABASE \"destdb_tmp\" RENAME TO \"destdb\"")

	// Cleanup
	assert.Contains(t, cmds, "DROP DATABASE IF EXISTS \"destdb_old\"")
}

func TestRestoreData_Postgres_PgRestoreFailure(t *testing.T) {
	_, executedCommands, teardown := setupTest()
	defer teardown()

	// Override runCmd specifically to simulate a fatal error during the pg_restore command
	runCmd = func(name string, args ...string) error {
		*executedCommands = append(*executedCommands, name+" "+strings.Join(args, " "))
		if name == "pg_restore" {
			return fmt.Errorf("mocked pg_restore failure")
		}
		return nil
	}

	cfg := SyncConfig{
		DBType:  "postgres",
		DestURI: "postgres://user:pass@localhost:5432/destdb",
	}

	err := restoreData(cfg, "/tmp/dummy_restore_dir")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pg_restore encountered an error")

	cmds := strings.Join(*executedCommands, "\n")

	// Verify that we still cleanly reverted the performance tunings even on failure
	assert.Contains(t, cmds, "ALTER DATABASE \"destdb_tmp\" RESET synchronous_commit", "Tunings should be reverted on pg_restore failure")

	// VERIFY: We absolutely MUST NOT perform the swap if the restore failed
	assert.NotContains(t, cmds, "ALTER DATABASE \"destdb_tmp\" RENAME TO \"destdb\"", "Should not execute rename/swap if pg_restore fails")
}
