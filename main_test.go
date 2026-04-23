package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Test Suite Global Setup
// ============================================================================

func TestMain(m *testing.M) {
	// Pre-flight environment check for Contract Tests
	if os.Getenv("RUN_CONTRACT_TESTS") == "1" {
		ctx := context.Background()
		awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("eu-west-1"))
		if err != nil {
			fmt.Printf("\n❌ [ENVIRONMENT ERROR] Failed to load AWS SDK config: %v\n\n", err)
			os.Exit(1) // Halts the test suite completely, preventing a "failed test" report
		}

		// Make a lightweight call to STS to verify the token is active
		stsClient := sts.NewFromConfig(awsCfg)
		_, err = stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			fmt.Printf("\n❌ [ENVIRONMENT ERROR] AWS credentials are invalid, expired, or missing.\nCannot proceed with contract tests. Please renew your AWS session.\n\nSTS Error Details: %v\n\n", err)
			os.Exit(1)
		}

		fmt.Println("✅ AWS Environment verified via STS. Proceeding with Contract Tests...")
	}

	// Run the actual test suite
	os.Exit(m.Run())
}

// ============================================================================
// Mock S3 Provider
// ============================================================================

type MockS3Provider struct {
	UploadArchiveFunc     func(ctx context.Context, bucket, key string, file *os.File) error
	DownloadArchiveFunc   func(ctx context.Context, bucket, key string, file *os.File) (int64, error)
	TagArchiveFunc        func(ctx context.Context, bucket, key, appName, dbEngine, dbName, timestamp string) error
	WritePointerFunc      func(ctx context.Context, bucket, key, targetKey string) error
	ReadPointerFunc       func(ctx context.Context, bucket, key string) (string, error)
	FindLatestArchiveFunc func(ctx context.Context, bucket, prefix string) (string, time.Time, error)

	Calls []string
}

func (m *MockS3Provider) UploadArchive(ctx context.Context, bucket, key string, file *os.File) error {
	m.Calls = append(m.Calls, "UploadArchive:"+key)
	if m.UploadArchiveFunc != nil {
		return m.UploadArchiveFunc(ctx, bucket, key, file)
	}
	return nil
}

func (m *MockS3Provider) DownloadArchive(ctx context.Context, bucket, key string, file *os.File) (int64, error) {
	m.Calls = append(m.Calls, "DownloadArchive:"+key)
	if m.DownloadArchiveFunc != nil {
		return m.DownloadArchiveFunc(ctx, bucket, key, file)
	}
	return 1024, nil
}

func (m *MockS3Provider) TagArchive(ctx context.Context, bucket, key string, appName, dbEngine, dbName, timestamp string) error {
	m.Calls = append(m.Calls, "TagArchive:"+key)
	if m.TagArchiveFunc != nil {
		return m.TagArchiveFunc(ctx, bucket, key, appName, dbEngine, dbName, timestamp)
	}
	return nil
}

func (m *MockS3Provider) WritePointer(ctx context.Context, bucket, key, targetKey string) error {
	m.Calls = append(m.Calls, "WritePointer:"+key+"->"+targetKey)
	if m.WritePointerFunc != nil {
		return m.WritePointerFunc(ctx, bucket, key, targetKey)
	}
	return nil
}

func (m *MockS3Provider) ReadPointer(ctx context.Context, bucket, key string) (string, error) {
	m.Calls = append(m.Calls, "ReadPointer:"+key)
	if m.ReadPointerFunc != nil {
		return m.ReadPointerFunc(ctx, bucket, key)
	}
	return "mocked_latest.tar.zst", nil
}

func (m *MockS3Provider) FindLatestArchive(ctx context.Context, bucket, prefix string) (string, time.Time, error) {
	m.Calls = append(m.Calls, "FindLatestArchive:"+prefix)
	if m.FindLatestArchiveFunc != nil {
		return m.FindLatestArchiveFunc(ctx, bucket, prefix)
	}
	return "guessed_latest.tar.zst", time.Now(), nil
}

// ============================================================================
// Test Setup Helper
// ============================================================================

// defaultGetS3Provider stores the original function so it can be restored
var defaultGetS3Provider = getS3Provider

func setupTest() (*MockS3Provider, *[]string, func()) {
	mockS3 := &MockS3Provider{}

	// Override the S3 Factory
	getS3Provider = func(ctx context.Context, region string) (S3Provider, error) {
		return mockS3, nil
	}

	// Override the CLI Runner
	var executedCommands []string
	var originalRunCmd = runCmd
	runCmd = func(name string, args ...string) error {
		executedCommands = append(executedCommands, name+" "+strings.Join(args, " "))

		// If testing the 'tar' compression, create a dummy file so os.Open succeeds
		if name == "tar" && len(args) > 1 && args[1] == "-cf" {
			archivePath := args[2]
			f, _ := os.Create(archivePath)
			f.Close()
		}
		return nil
	}

	// Override output command runner for table fetching
	var originalRunCmdWithOutput = runCmdWithOutput
	runCmdWithOutput = func(name string, args ...string) ([]byte, error) {
		executedCommands = append(executedCommands, name+" "+strings.Join(args, " "))

		// Specific mock behavior for MySQL testing schema fetches
		cmdStr := strings.Join(args, " ")
		if strings.Contains(cmdStr, "SELECT table_name FROM information_schema.tables") {
			if strings.Contains(cmdStr, "destdb_tmp") {
				if strings.Contains(cmdStr, "VIEW") {
					return []byte("view1\nview2\n"), nil
				}
				// Mock the restored tables
				return []byte("users\nposts\ncomments\n"), nil
			} else if strings.Contains(cmdStr, "destdb") {
				if strings.Contains(cmdStr, "VIEW") {
					return []byte(""), nil
				}
				// Mock the currently live tables (to test removal of obsolete tables)
				return []byte("users\nposts\nold_table\n"), nil
			}
		}
		return []byte(""), nil
	}

	// Override piping logic to prevent OS-level process blocking during tests
	var originalRunPipe = runPipe
	runPipe = func(cmd1 string, args1 []string, cmd2 string, args2 []string) error {
		executedCommands = append(executedCommands, cmd1+" "+strings.Join(args1, " ")+" | "+cmd2+" "+strings.Join(args2, " "))
		return nil
	}

	// Return a teardown function
	teardown := func() {
		getS3Provider = defaultGetS3Provider
		runCmd = originalRunCmd
		runCmdWithOutput = originalRunCmdWithOutput
		runPipe = originalRunPipe
	}

	return mockS3, &executedCommands, teardown
}

// ============================================================================
// Smoke Tests
// ============================================================================

func TestSmoke_RequiredBinaries(t *testing.T) {
	if os.Getenv("RUN_SMOKE_TESTS") != "1" {
		t.Skip("Skipping smoke tests; set RUN_SMOKE_TESTS=1 to run")
	}

	requiredBins := []string{
		"tar",
		"mydumper",
		"myloader",
		"mysql",
		"pg_dump",
		"pg_restore",
		"psql",
		"mongodump",
		"mongorestore",
		"mongo",
	}

	for _, bin := range requiredBins {
		t.Run(bin, func(t *testing.T) {
			_, err := exec.LookPath(bin)
			assert.NoError(t, err, "CRITICAL: Required binary '%s' is missing from $PATH. Is it installed in the Dockerfile?", bin)
		})
	}
}

// ============================================================================
// CLI Pipeline Error Pathways & Deferred Cleanup Tests
// ============================================================================

func TestCLI_Backup_DeferredCleanup_OnSuccess(t *testing.T) {
	mockS3, _, teardown := setupTest()
	defer teardown()

	// Simulate tools generating files that need to be cleaned up
	os.MkdirAll("/tmp/pg_dump_dir", 0755)
	os.WriteFile("/tmp/pg_dump_dir/dump.sql", []byte("mocked"), 0644)

	os.MkdirAll("/tmp/pg_dump_dir_transformed", 0755)
	os.WriteFile("/tmp/pg_dump_dir_transformed/dump.sql", []byte("mocked"), 0644)

	os.WriteFile("/tmp/pg_dump_dir.tar.zst", []byte("mocked archive"), 0644)

	app := NewApp()
	err := app.Run(context.Background(), []string{
		"db-sync", "backup",
		"--type", "postgres",
		"--source", "postgres://user:pass@localhost:5432/db",
		"--s3-bucket", "test-bucket",
		"--transform-script", "dummy.sql",
		"--transform-uri", "postgres://user:pass@localhost:5432/sidecar",
	})
	require.NoError(t, err)

	assert.NotEmpty(t, mockS3.Calls, "S3 upload should have been triggered")

	// Ensure Deferred Cleanup successfully wiped the working directories
	_, err1 := os.Stat("/tmp/pg_dump_dir")
	assert.True(t, os.IsNotExist(err1), "raw dump directory was not cleaned up!")

	_, err2 := os.Stat("/tmp/pg_dump_dir_transformed")
	assert.True(t, os.IsNotExist(err2), "transformed dump directory was not cleaned up!")

	_, err3 := os.Stat("/tmp/pg_dump_dir.tar.zst")
	assert.True(t, os.IsNotExist(err3), "archive file was not cleaned up!")
}

func TestCLI_Backup_DeferredCleanup_OnFailure(t *testing.T) {
	_, _, teardown := setupTest()
	defer teardown()

	os.MkdirAll("/tmp/pg_dump_dir", 0755)
	os.WriteFile("/tmp/pg_dump_dir/dump.sql", []byte("mocked"), 0644)

	// Intercept and force an error to simulate a fatal DB export crash
	originalRunCmd := runCmd
	runCmd = func(name string, args ...string) error {
		return fmt.Errorf("mocked pg_dump fatal error")
	}
	defer func() { runCmd = originalRunCmd }()

	app := NewApp()
	err := app.Run(context.Background(), []string{
		"db-sync", "backup",
		"--type", "postgres",
		"--source", "postgres://user:pass@localhost:5432/db",
		"--s3-bucket", "test-bucket",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "export phase failed")
	assert.Contains(t, err.Error(), "mocked pg_dump fatal error")

	// Verify that even though the app violently crashed, the defer block caught the mess
	_, statErr := os.Stat("/tmp/pg_dump_dir")
	assert.True(t, os.IsNotExist(statErr), "raw dump directory leaked! Cleanup failed to trigger on error.")
}

func TestCLI_Backup_TransformURI_Validation(t *testing.T) {
	_, _, teardown := setupTest()
	defer teardown()

	// Provide a dummy script so it doesn't short circuit on the os.Stat check
	script, _ := os.CreateTemp("", "transform_test*.sql")
	script.Close()
	defer os.Remove(script.Name())

	app := NewApp()
	err := app.Run(context.Background(), []string{
		"db-sync", "backup",
		"--type", "postgres",
		"--source", "postgres://user:pass@localhost:5432/db",
		"--s3-bucket", "test-bucket",
		"--transform-script", script.Name(),
		// DELIBERATELY MISSING --transform-uri
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "transform phase failed")
	assert.Contains(t, err.Error(), "transform URI is required") // Should catch the missing URI instantly
}

func TestCLI_Restore_DeferredCleanup_OnSuccess(t *testing.T) {
	_, _, teardown := setupTest()
	defer teardown()

	extractPath := "/tmp/postgres_testdb_downloaded"
	os.MkdirAll(extractPath, 0755)
	os.WriteFile(extractPath+"/dump.sql", []byte("mocked payload"), 0644)

	app := NewApp()
	err := app.Run(context.Background(), []string{
		"db-sync", "restore",
		"--type", "postgres",
		"--db-name", "testdb",
		"--dest", "postgres://user:pass@localhost:5432/dest",
		"--s3-bucket", "test-bucket",
	})
	require.NoError(t, err)

	_, statErr := os.Stat(extractPath)
	assert.True(t, os.IsNotExist(statErr), "restored download directory was not cleaned up!")
}
