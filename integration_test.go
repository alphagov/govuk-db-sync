package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Helper to pull the dynamic docker-compose container names, or fallback to localhost
func getEnvOrDefault(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

// ============================================================================
// End-to-End Database Integration Tests
// ============================================================================
// These tests execute real database binaries against the local Docker Compose
// sidecar databases to verify the entire pipeline (Export -> Transform -> Restore).
// ============================================================================

func TestIntegration_PostgresPipeline(t *testing.T) {
	if os.Getenv("RUN_INTEGRATION_TESTS") != "1" {
		t.Skip("Skipping Integration tests; set RUN_INTEGRATION_TESTS=1 and ensure docker-compose sidecars are running.")
	}

	// Ensure we are using the REAL shell command executors, not mocks
	runCmd = defaultRunCmd
	runCmdWithOutput = defaultRunCmdWithOutput
	runPipe = defaultRunPipe

	// 1. Setup Test Configuration & URIs
	adminURI := getEnvOrDefault("TEST_PG_ADMIN_URI", "postgres://postgres:secret@localhost:5432/postgres?sslmode=disable")
	sourceURI := getEnvOrDefault("TEST_PG_SOURCE_URI", "postgres://postgres:secret@localhost:5432/source_db?sslmode=disable")
	transformURI := getEnvOrDefault("TEST_PG_TRANSFORM_URI", "postgres://postgres:secret@localhost:5432/transform_db?sslmode=disable")
	destURI := getEnvOrDefault("TEST_PG_DEST_URI", "postgres://postgres:secret@localhost:5432/dest_db?sslmode=disable")

	cfg := SyncConfig{
		DBType:       "postgres",
		DBName:       "source_db",
		SourceURI:    sourceURI,
		TransformURI: transformURI,
		DestURI:      destURI,
		Threads:      "2",
	}

	dataPath := "/tmp/integration_pg_dump"
	defer os.RemoveAll(dataPath)
	defer os.RemoveAll(dataPath + "_transformed")

	// 2. Prepare Source Database with Dummy PII
	t.Log("Setting up source database and injecting dummy data...")
	_ = runCmd("psql", "--dbname", adminURI, "-c", "DROP DATABASE IF EXISTS source_db WITH (FORCE);")
	require.NoError(t, runCmd("psql", "--dbname", adminURI, "-c", "CREATE DATABASE source_db;"))

	setupSQL := `
	CREATE TABLE users (id SERIAL PRIMARY KEY, name VARCHAR(50), email VARCHAR(50));
	INSERT INTO users (name, email) VALUES ('John Smith', 'john.smith@example.gov.uk'), ('Guy Incognito', 'guy.incognito@example.gov.uk');
	`
	require.NoError(t, runCmd("psql", "--dbname", sourceURI, "-c", setupSQL))

	// 3. Create a Transform Script to redact the PII
	transformScript, _ := os.CreateTemp("", "transform-integration-*.sql")
	defer os.Remove(transformScript.Name())
	_, _ = transformScript.WriteString("UPDATE users SET name = 'REDACTED', email = CONCAT('user_', id, '@anonymised.gov.uk');")
	transformScript.Close()
	cfg.TransformScript = transformScript.Name()

	// 4. Run Pipeline: Export
	t.Log("Executing Export Phase...")
	err := exportData(cfg, dataPath)
	require.NoError(t, err, "Export phase failed")

	// 5. Run Pipeline: Transform
	t.Log("Executing Transform Phase...")
	transformedPath, err := transformData(cfg, dataPath)
	require.NoError(t, err, "Transform phase failed")

	// 6. Run Pipeline: Restore
	t.Log("Executing Restore Phase...")
	err = restoreData(cfg, transformedPath)
	require.NoError(t, err, "Restore phase failed")

	// 7. Verify the Atomic Swap and Data Sanitisation
	t.Log("Validating restored data in destination database...")
	out, err := runCmdWithOutput("psql", "--dbname", destURI, "-t", "-c", "SELECT email FROM users ORDER BY id ASC;")
	require.NoError(t, err, "Failed to query destination database")

	results := strings.TrimSpace(string(out))
	assert.Contains(t, results, "user_1@anonymised.gov.uk")
	assert.Contains(t, results, "user_2@anonymised.gov.uk")
	assert.NotContains(t, results, "john.smith@example.gov.uk", "PII leaked through the integration test pipeline!")

	// 8. Teardown
	t.Log("Cleaning up integration databases...")
	_ = runCmd("psql", "--dbname", adminURI, "-c", "DROP DATABASE IF EXISTS source_db WITH (FORCE);")
	_ = runCmd("psql", "--dbname", adminURI, "-c", "DROP DATABASE IF EXISTS dest_db WITH (FORCE);")
	_ = runCmd("psql", "--dbname", adminURI, "-c", "DROP DATABASE IF EXISTS transform_db WITH (FORCE);")
}

func TestIntegration_MysqlPipeline(t *testing.T) {
	if os.Getenv("RUN_INTEGRATION_TESTS") != "1" {
		t.Skip("Skipping Integration tests; set RUN_INTEGRATION_TESTS=1 and ensure docker-compose sidecars are running.")
	}

	runCmd = defaultRunCmd
	runCmdWithOutput = defaultRunCmdWithOutput
	runPipe = defaultRunPipe

	// 1. Setup Test Configuration
	sourceURI := getEnvOrDefault("TEST_MYSQL_SOURCE_URI", "mysql://root:secret@127.0.0.1:3306/source_db")
	transformURI := getEnvOrDefault("TEST_MYSQL_TRANSFORM_URI", "mysql://root:secret@127.0.0.1:3306/transform_db")
	destURI := getEnvOrDefault("TEST_MYSQL_DEST_URI", "mysql://root:secret@127.0.0.1:3306/dest_db")

	cfg := SyncConfig{
		DBType:       "mysql",
		DBName:       "source_db",
		SourceURI:    sourceURI,
		TransformURI: transformURI,
		DestURI:      destURI,
		Threads:      "2",
	}

	dataPath := "/tmp/integration_mysql_dump"
	defer os.RemoveAll(dataPath)
	defer os.RemoveAll(dataPath + "_transformed")

	// 2. Prepare Source Database
	t.Log("Setting up MySQL source database...")

	// Dynamically parse URIs to construct the MySQL CLI args safely
	mysqlExec := func(uriStr string, useDB bool, query string, isValidation bool) ([]byte, error) {
		u, _ := url.Parse(uriStr)
		host := u.Hostname()
		port := u.Port()
		if port == "" {
			port = "3306"
		}
		user := u.User.Username()
		pass, _ := u.User.Password()
		db := extractDBName(u.String())

		args := []string{"--host", host, "--port", port, "--user", user, fmt.Sprintf("--password=%s", pass)}
		if useDB && db != "" {
			args = append(args, db)
		}
		if isValidation {
			args = append(args, "-N", "-B")
		}
		args = append(args, "-e", query)

		if isValidation {
			return runCmdWithOutput("mysql", args...)
		}
		return nil, runCmd("mysql", args...)
	}

	_, _ = mysqlExec(sourceURI, false, "DROP DATABASE IF EXISTS source_db;", false)
	_, err := mysqlExec(sourceURI, false, "CREATE DATABASE source_db;", false)
	require.NoError(t, err)

	setupSQL := `
	CREATE TABLE users (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50), email VARCHAR(50));
	INSERT INTO users (name, email) VALUES ('John Smith', 'john.smith@example.gov.uk'), ('Guy Incognito', 'guy.incognito@example.gov.uk');
	`
	_, err = mysqlExec(sourceURI, true, setupSQL, false)
	require.NoError(t, err)

	// 3. Create Transform Script
	transformScript, _ := os.CreateTemp("", "transform-mysql-*.sql")
	defer os.Remove(transformScript.Name())
	_, _ = transformScript.WriteString("UPDATE users SET name = 'REDACTED', email = CONCAT('user_', id, '@anonymised.gov.uk');")
	transformScript.Close()
	cfg.TransformScript = transformScript.Name()

	// 4. Run Pipeline
	t.Log("Executing MySQL Pipeline...")
	require.NoError(t, exportData(cfg, dataPath), "Export phase failed")
	transformedPath, err := transformData(cfg, dataPath)
	require.NoError(t, err, "Transform phase failed")
	require.NoError(t, restoreData(cfg, transformedPath), "Restore phase failed")

	// 5. Verify the Swap and Sanitisation
	t.Log("Validating MySQL restored data...")
	out, err := mysqlExec(destURI, true, "SELECT email FROM users ORDER BY id ASC;", true)
	require.NoError(t, err)

	results := strings.TrimSpace(string(out))
	assert.Contains(t, results, "user_1@anonymised.gov.uk")
	assert.Contains(t, results, "user_2@anonymised.gov.uk")
	assert.NotContains(t, results, "john.smith@example.gov.uk")

	// 6. Teardown
	_, _ = mysqlExec(sourceURI, false, "DROP DATABASE IF EXISTS source_db;", false)
	_, _ = mysqlExec(destURI, false, "DROP DATABASE IF EXISTS dest_db;", false)
	_, _ = mysqlExec(transformURI, false, "DROP DATABASE IF EXISTS transform_db;", false)
}

func TestIntegration_DocumentDBPipeline(t *testing.T) {
	if os.Getenv("RUN_INTEGRATION_TESTS") != "1" {
		t.Skip("Skipping Integration tests; set RUN_INTEGRATION_TESTS=1 and ensure docker-compose sidecars are running.")
	}

	runCmd = defaultRunCmd
	runCmdWithOutput = defaultRunCmdWithOutput
	runPipe = defaultRunPipe

	// Setup Test Configuration
	sourceURI := getEnvOrDefault("TEST_MONGO_SOURCE_URI", "mongodb://mongoadmin:secret@localhost:27017/source_db?authSource=admin")
	transformURI := getEnvOrDefault("TEST_MONGO_TRANSFORM_URI", "mongodb://mongoadmin:secret@localhost:27017/transform_db?authSource=admin")
	destURI := getEnvOrDefault("TEST_MONGO_DEST_URI", "mongodb://mongoadmin:secret@localhost:27017/dest_db?authSource=admin")

	cfg := SyncConfig{
		DBType:        "documentdb",
		DBName:        "source_db",
		DocDBSourceDB: "source_db", // Required for DocDB namespace mapping on restore
		SourceURI:     sourceURI,
		TransformURI:  transformURI,
		DestURI:       destURI,
		Threads:       "2",
	}

	dataPath := "/tmp/integration_mongo_dump"
	defer os.RemoveAll(dataPath)
	defer os.RemoveAll(dataPath + "_transformed")

	// Prepare Source Database
	t.Log("Setting up DocumentDB source database...")
	_ = runCmd("mongo", cfg.SourceURI, "--eval", "db.dropDatabase()")

	insertJS := `db.users.insertMany([{_id: 1, name: "John Smith", email: "john.smith@example.gov.uk"}, {_id: 2, name: "Guy Incognito", email: "guy.incognito@example.gov.uk"}]);`
	require.NoError(t, runCmd("mongo", cfg.SourceURI, "--eval", insertJS))

	// Create Transform Script
	transformScript, _ := os.CreateTemp("", "transform-mongo-*.js")
	defer os.Remove(transformScript.Name())

	// MongoDB js script to iterate and update documents
	updateJS := `
	db.users.find().forEach(function(doc) {
		doc.name = 'REDACTED';
		doc.email = 'user_' + doc._id + '@anonymised.gov.uk';
		db.users.save(doc);
	});
	`
	_, _ = transformScript.WriteString(updateJS)
	transformScript.Close()
	cfg.TransformScript = transformScript.Name()

	// Run Pipeline
	t.Log("Executing DocumentDB Pipeline...")
	require.NoError(t, exportData(cfg, dataPath), "Export phase failed")
	transformedPath, err := transformData(cfg, dataPath)
	require.NoError(t, err, "Transform phase failed")
	require.NoError(t, restoreData(cfg, transformedPath), "Restore phase failed")

	// Verify the Restore and Sanitisation
	t.Log("Validating DocumentDB restored data...")
	verifyJS := `db.users.find().sort({_id: 1}).forEach(function(u){print(u.email)})`
	out, err := runCmdWithOutput("mongo", "--quiet", cfg.DestURI, "--eval", verifyJS)
	require.NoError(t, err)

	results := strings.TrimSpace(string(out))
	assert.Contains(t, results, "user_1@anonymised.gov.uk")
	assert.Contains(t, results, "user_2@anonymised.gov.uk")
	assert.NotContains(t, results, "john.smith@example.gov.uk")

	// Teardown
	_ = runCmd("mongo", cfg.SourceURI, "--eval", "db.dropDatabase()")
	_ = runCmd("mongo", cfg.DestURI, "--eval", "db.dropDatabase()")
	_ = runCmd("mongo", cfg.TransformURI, "--eval", fmt.Sprintf("db.getSiblingDB('%s').dropDatabase()", cfg.DBName))
}
