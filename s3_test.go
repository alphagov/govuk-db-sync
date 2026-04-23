package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUploadToS3(t *testing.T) {
	mockS3, _, teardown := setupTest()
	defer teardown()

	cfg := SyncConfig{
		AppName:  "release",
		DBType:   "postgres",
		DBName:   "release_production",
		S3Bucket: "test-bucket",
		S3Path:   "release_postgres/release",
		S3Region: "eu-west-1",
	}

	os.MkdirAll("/tmp/pg_dump_dir", 0755)
	defer os.RemoveAll("/tmp/pg_dump_dir")

	_, err := uploadToS3(cfg, "/tmp/pg_dump_dir")
	require.NoError(t, err)

	require.Len(t, mockS3.Calls, 3)
	assert.True(t, strings.HasPrefix(mockS3.Calls[0], "UploadArchive:"))
	assert.True(t, strings.HasPrefix(mockS3.Calls[1], "TagArchive:"))
	assert.True(t, strings.HasPrefix(mockS3.Calls[2], "WritePointer:release_postgres/release_latest.txt"))
}

func TestUploadToS3_UploadFailure(t *testing.T) {
	mockS3, _, teardown := setupTest()
	defer teardown()

	mockS3.UploadArchiveFunc = func(ctx context.Context, bucket, key string, file *os.File) error {
		return fmt.Errorf("simulated S3 upload error")
	}

	cfg := SyncConfig{
		AppName:  "release",
		DBType:   "postgres",
		DBName:   "release_production",
		S3Bucket: "test-bucket",
		S3Path:   "release_postgres/release",
		S3Region: "eu-west-1",
	}

	os.MkdirAll("/tmp/pg_dump_dir_fail", 0755)
	defer os.RemoveAll("/tmp/pg_dump_dir_fail")

	_, err := uploadToS3(cfg, "/tmp/pg_dump_dir_fail")
	require.Error(t, err)

	require.Len(t, mockS3.Calls, 1, "expected exactly 1 S3 call (UploadArchive)")
	assert.True(t, strings.HasPrefix(mockS3.Calls[0], "UploadArchive:"))
}

func TestDownloadFromS3_ReadsPointer(t *testing.T) {
	mockS3, _, teardown := setupTest()
	defer teardown()

	mockS3.ReadPointerFunc = func(ctx context.Context, bucket, key string) (string, error) {
		return "found_via_pointer.tar.zst", nil
	}

	cfg := SyncConfig{
		DBType:   "mysql",
		DBName:   "default",
		S3Bucket: "test-bucket",
	}

	path, _, err := downloadFromS3(cfg)
	require.NoError(t, err)
	assert.Equal(t, "/tmp/mysql_default_downloaded", path)

	if !strings.HasPrefix(mockS3.Calls[0], "ReadPointer:") {
		t.Errorf("expected ReadPointer to be called first, got: %s", mockS3.Calls[0])
	}
	if mockS3.Calls[1] != "DownloadArchive:found_via_pointer.tar.zst" {
		t.Errorf("expected DownloadArchive to use pointer key, got: %s", mockS3.Calls[1])
	}
}

func TestDownloadFromS3_FallbackFailure(t *testing.T) {
	mockS3, _, teardown := setupTest()
	defer teardown()

	mockS3.ReadPointerFunc = func(ctx context.Context, bucket, key string) (string, error) {
		return "", fmt.Errorf("NoSuchKey")
	}

	cfg := SyncConfig{
		DBType:   "postgres",
		DBName:   "release",
		S3Bucket: "test-bucket",
		S3Path:   "release_postgres/release",
	}

	_, _, err := downloadFromS3(cfg)
	require.Error(t, err)

	// Should fail immediately after ReadPointer since we removed the guesswork logic!
	assert.Len(t, mockS3.Calls, 1)
}

func TestContract_S3Connectivity(t *testing.T) {
	if os.Getenv("RUN_CONTRACT_TESTS") != "1" {
		t.Skip("Skipping S3 contract test; set RUN_CONTRACT_TESTS=1 to run")
	}

	bucket := os.Getenv("TEST_S3_BUCKET")
	require.NotEmpty(t, bucket, "TEST_S3_BUCKET environment variable must be set for contract tests")

	ctx := context.Background()

	provider, err := defaultGetS3Provider(ctx, "eu-west-1")
	require.NoError(t, err, "Failed to initialize the real AWS S3 provider")

	dummyPath := "/tmp/contract_test_archive.tar.zst"
	err = os.WriteFile(dummyPath, []byte("govuk contract test data validation"), 0644)
	require.NoError(t, err)
	defer os.Remove(dummyPath)

	file, err := os.Open(dummyPath)
	require.NoError(t, err)
	defer file.Close()

	testKey := fmt.Sprintf("test_contract_tests/test_payload_%d.tar.zst", time.Now().Unix())

	t.Logf("Attempting real upload to s3://%s/%s...", bucket, testKey)
	err = provider.UploadArchive(ctx, bucket, testKey, file)
	assert.NoError(t, err, "Failed to upload archive to S3. Check AWS credentials and bucket permissions.")

	t.Log("Attempting real tagging application...")
	err = provider.TagArchive(ctx, bucket, testKey, "contract_test_app", "mock_db", "testdb", time.Now().Format(time.RFC3339))
	assert.NoError(t, err, "Failed to tag the archive in S3.")
}
