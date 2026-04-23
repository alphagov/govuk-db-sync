package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExportData_DocumentDB(t *testing.T) {
	_, executedCommands, teardown := setupTest()
	defer teardown()

	cfg := SyncConfig{
		DBType:    "documentdb",
		DBName:    "test_app_production",
		SourceURI: "mongodb://user:pass@localhost:27017/test_app_production",
	}

	dummyPath := "/tmp/test_docdb_dump"
	defer os.RemoveAll(dummyPath)

	err := exportData(cfg, dummyPath)
	require.NoError(t, err)

	require.NotEmpty(t, *executedCommands)
	cmdRan := (*executedCommands)[0]
	assert.Contains(t, cmdRan, "mongodump")
	assert.Contains(t, cmdRan, fmt.Sprintf("--out=%s", dummyPath))
	assert.Contains(t, cmdRan, "--numParallelCollections")
}

func TestTransformData_DocumentDB(t *testing.T) {
	tempScript, _ := os.CreateTemp("", "transform-*.js")
	defer os.Remove(tempScript.Name())

	t.Run("explicit DBName", func(t *testing.T) {
		_, executedCommands, teardown := setupTest()
		defer teardown()

		cfg := SyncConfig{
			DBType:          "documentdb",
			DBName:          "test_app_production",
			TransformURI:    "mongodb://user:pass@localhost:27017/test_app_sidecar",
			TransformScript: tempScript.Name(),
		}

		path, err := transformData(cfg, "/tmp/test_docdb_dump")
		require.NoError(t, err)
		assert.Equal(t, "/tmp/test_docdb_dump_transformed", path)

		require.Len(t, *executedCommands, 4, "expected 4 commands for DocDB transform")

		// Drop DB (Must match original production database name, not the sidecar name)
		assert.Contains(t, (*executedCommands)[0], "mongo")
		assert.Contains(t, (*executedCommands)[0], "db.getSiblingDB('test_app_production').dropDatabase()")

		// mongorestore with original namespace
		assert.Contains(t, (*executedCommands)[1], "mongorestore")
		assert.Contains(t, (*executedCommands)[1], "--nsInclude=test_app_production.*")
		assert.NotContains(t, (*executedCommands)[1], "--nsFrom", "should not contain nsFrom renaming during transform phase")

		// mongo run script
		assert.Contains(t, (*executedCommands)[2], "mongo")
		assert.Contains(t, (*executedCommands)[2], tempScript.Name())

		// mongodump sanitized
		assert.Contains(t, (*executedCommands)[3], "mongodump")
		assert.Contains(t, (*executedCommands)[3], "--out=/tmp/test_docdb_dump_transformed")
		assert.Contains(t, (*executedCommands)[3], "--numParallelCollections")
	})

	t.Run("DBName from URI", func(t *testing.T) {
		_, executedCommands, teardown := setupTest()
		defer teardown()

		cfg := SyncConfig{
			DBType:          "documentdb",
			SourceURI:       "mongodb://user:pass@localhost:27017/urldb",
			TransformURI:    "mongodb://user:pass@localhost:27017/test_app_sidecar",
			TransformScript: tempScript.Name(),
		}

		path, err := transformData(cfg, "/tmp/test_docdb_dump")
		require.NoError(t, err)
		assert.Equal(t, "/tmp/test_docdb_dump_transformed", path)

		require.Len(t, *executedCommands, 4, "expected 4 commands for DocDB transform")
		assert.Contains(t, (*executedCommands)[0], "db.getSiblingDB('urldb').dropDatabase()")
	})

	t.Run("missing DBName", func(t *testing.T) {
		_, _, teardown := setupTest()
		defer teardown()

		cfg := SyncConfig{
			DBType:          "documentdb",
			SourceURI:       "mongodb://user:pass@localhost:27017/",
			TransformURI:    "mongodb://user:pass@localhost:27017/test_app_sidecar",
			TransformScript: tempScript.Name(),
		}

		_, err := transformData(cfg, "/tmp/test_docdb_dump")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "DocumentDB transform requires a target database name")
	})
}

func TestRestoreData_DocumentDB_NamespaceInjection(t *testing.T) {
	_, executedCommands, teardown := setupTest()
	defer teardown()

	cfg := SyncConfig{
		DBType:        "documentdb",
		DocDBSourceDB: "test_app_production",
		DestURI:       "mongodb://user:pass@localhost:27017/test_app_integration",
	}

	err := restoreData(cfg, "/tmp/test_docdb_restore")
	require.NoError(t, err)

	require.NotEmpty(t, *executedCommands)
	cmdRan := (*executedCommands)[0]
	assert.Contains(t, cmdRan, "mongorestore")

	assert.Contains(t, cmdRan, "--nsInclude=test_app_production.*")
	assert.Contains(t, cmdRan, "--nsFrom=test_app_production.*")
	assert.Contains(t, cmdRan, "--nsTo=test_app_integration.*")

	// Ensure the implicit database is stripped from the URI to prevent mongorestore --db/--nsInclude conflicts
	assert.Contains(t, cmdRan, "--uri mongodb://user:pass@localhost:27017/")
	assert.NotContains(t, cmdRan, "mongodb://user:pass@localhost:27017/test_app_integration")
}
