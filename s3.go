package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/dustin/go-humanize"
	"github.com/rs/zerolog/log"
)

// S3Provider defines an interface for all AWS S3 operations, allowing them to be mocked in tests.
type S3Provider interface {
	UploadArchive(ctx context.Context, bucket, key string, file *os.File) error
	DownloadArchive(ctx context.Context, bucket, key string, file *os.File) (int64, error)
	TagArchive(ctx context.Context, bucket, key string, appName, dbEngine, dbName, timestamp string) error
	WritePointer(ctx context.Context, bucket, key, targetKey string) error
	ReadPointer(ctx context.Context, bucket, key string) (string, error)
}

// getS3Provider is a mockable factory for the S3 interface
var getS3Provider = func(ctx context.Context, region string) (S3Provider, error) {
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS SDK config: %w", err)
	}

	s3Client := s3.NewFromConfig(awsCfg)
	tmClient := transfermanager.New(s3Client, func(o *transfermanager.Options) {
		o.PartSizeBytes = 64 * 1024 * 1024
		o.Concurrency = 10
	})

	return &awsS3Provider{s3Client: s3Client, tmClient: tmClient}, nil
}

type awsS3Provider struct {
	s3Client *s3.Client
	tmClient *transfermanager.Client
}

func (a *awsS3Provider) UploadArchive(ctx context.Context, bucket, key string, file *os.File) error {
	_, err := a.tmClient.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   file,
	})
	return err
}

func (a *awsS3Provider) DownloadArchive(ctx context.Context, bucket, key string, file *os.File) (int64, error) {
	result, err := a.tmClient.DownloadObject(ctx, &transfermanager.DownloadObjectInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(key),
		WriterAt: file,
	})
	if err != nil {
		return 0, err
	}
	return *result.ContentLength, nil
}

func (a *awsS3Provider) TagArchive(ctx context.Context, bucket, key string, appName, dbEngine, dbName, timestamp string) error {
	_, err := a.s3Client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Tagging: &types.Tagging{
			TagSet: []types.Tag{
				{Key: aws.String("app_name"), Value: aws.String(appName)},
				{Key: aws.String("db_engine"), Value: aws.String(dbEngine)},
				{Key: aws.String("db_name"), Value: aws.String(dbName)},
				{Key: aws.String("timestamp"), Value: aws.String(timestamp)},
			},
		},
	})
	return err
}

func (a *awsS3Provider) WritePointer(ctx context.Context, bucket, key, targetKey string) error {
	_, err := a.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(targetKey),
	})
	return err
}

func (a *awsS3Provider) ReadPointer(ctx context.Context, bucket, key string) (string, error) {
	out, err := a.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return "", err
	}
	defer out.Body.Close()
	bodyBytes, err := io.ReadAll(out.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(bodyBytes)), nil
}

// uploadToS3 compresses the data directory into a tarball and uploads it to S3 safely
// Returns the size of the uploaded archive file in bytes.
func uploadToS3(cfg SyncConfig, dataPath string) (int64, error) {
	archivePath := dataPath + ".tar.zst"

	baseS3Path := strings.TrimSuffix(cfg.S3Path, ".tar.zst")
	baseS3Path = strings.TrimSuffix(baseS3Path, ".tar.gz")

	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	timestampedKey := fmt.Sprintf("%s_%s.tar.zst", baseS3Path, timestamp)
	latestKey := fmt.Sprintf("%s_latest.txt", baseS3Path)

	log.Info().Msgf("Compressing %s into %s using ZSTD...\n", dataPath, archivePath)
	if cfg.DryRun {
		log.Warn().Msgf("[DRY RUN] Would tar --zstd -cf %s -C %s .\n", archivePath, dataPath)
		log.Warn().Msgf("[DRY RUN] Would upload %s to s3://%s/%s (Region: %s)\n", archivePath, cfg.S3Bucket, timestampedKey, cfg.S3Region)
		log.Warn().Msgf("[DRY RUN] Would tag %s with app_name=%s, db_engine=%s\n", timestampedKey, cfg.AppName, cfg.DBType)
		log.Warn().Msgf("[DRY RUN] On success, would create pointer s3://%s/%s containing '%s'\n", cfg.S3Bucket, latestKey, timestampedKey)
		return 0, nil
	}

	// Compress the directory into a zstd tarball
	if err := runCmd("tar", "--zstd", "-cf", archivePath, "-C", dataPath, "."); err != nil {
		return 0, fmt.Errorf("failed to compress payload: %w", err)
	}
	defer os.Remove(archivePath)

	log.Info().Msgf("Uploading %s to s3://%s/%s in region %s...\n", archivePath, cfg.S3Bucket, timestampedKey, cfg.S3Region)

	ctx := context.Background()
	provider, err := getS3Provider(ctx, cfg.S3Region)
	if err != nil {
		return 0, err
	}

	file, err := os.Open(archivePath)
	if err != nil {
		return 0, fmt.Errorf("failed to open archive for upload: %w", err)
	}
	defer file.Close()

	stat, _ := file.Stat()
	archiveSize := int64(0)
	if stat != nil {
		archiveSize = stat.Size()
	}

	if err := provider.UploadArchive(ctx, cfg.S3Bucket, timestampedKey, file); err != nil {
		log.Error().Err(err).Msgf("Failed to upload to S3: %s", err.Error())
		return 0, fmt.Errorf("failed to upload to S3: %w", err)
	}

	log.Info().Msgf("Tagging object %s with metadata...", timestampedKey)
	if err := provider.TagArchive(ctx, cfg.S3Bucket, timestampedKey, cfg.AppName, cfg.DBType, cfg.DBName, timestamp); err != nil {
		log.Warn().Err(err).Msg("Failed to tag the uploaded object (continuing anyway)")
	}

	// Write the pointer file only after successful upload
	log.Info().Msgf("Upload successful! Writing pointer file to s3://%s/%s", cfg.S3Bucket, latestKey)
	if err := provider.WritePointer(ctx, cfg.S3Bucket, latestKey, timestampedKey); err != nil {
		return 0, fmt.Errorf("failed to write latest pointer file: %w", err)
	}

	return archiveSize, nil
}

// downloadFromS3 downloads the archive from S3, extracts it, and returns the local path and download size
func downloadFromS3(cfg SyncConfig) (string, int64, error) {
	archivePath := fmt.Sprintf("/tmp/%s_%s_downloaded.archive", cfg.DBType, cfg.DBName)
	extractPath := fmt.Sprintf("/tmp/%s_%s_downloaded", cfg.DBType, cfg.DBName)

	baseS3Path := strings.TrimSuffix(cfg.S3Path, ".tar.zst")
	baseS3Path = strings.TrimSuffix(baseS3Path, ".tar.gz")
	latestKey := fmt.Sprintf("%s_latest.txt", baseS3Path)

	log.Info().Msgf("Initiating download sequence for s3://%s prefix %s in region %s...\n", cfg.S3Bucket, baseS3Path, cfg.S3Region)

	if cfg.DryRun {
		log.Warn().Msgf("[DRY RUN] Would attempt to read pointer from s3://%s/%s\n", cfg.S3Bucket, latestKey)
		log.Warn().Msgf("[DRY RUN] Would download determined archive to %s\n", archivePath)
		return extractPath, 0, nil
	}

	ctx := context.Background()
	provider, err := getS3Provider(ctx, cfg.S3Region)
	if err != nil {
		return "", 0, err
	}

	file, err := os.Create(archivePath)
	if err != nil {
		return "", 0, fmt.Errorf("failed to create local file for download: %w", err)
	}
	defer file.Close()

	var targetKey string

	// Attempt to read the pointer file
	pointerVal, err := provider.ReadPointer(ctx, cfg.S3Bucket, latestKey)
	if err != nil {
		log.Error().Err(err).Msgf("Pointer file %s not found or unreadable.", latestKey)
		return "", 0, fmt.Errorf("pointer file missing or unreadable, assuming backup is invalid/incomplete: %w", err)
	}

	targetKey = pointerVal
	log.Info().Msgf("Found pointer file %s. Target archive: %s", latestKey, targetKey)

	downloadLen, err := provider.DownloadArchive(ctx, cfg.S3Bucket, targetKey, file)
	if err != nil {
		return "", 0, fmt.Errorf("failed to download from S3: %w", err)
	}

	log.Info().Msgf("Downloaded %s from S3.", humanize.Bytes(uint64(downloadLen)))

	// Make sure the target directory exists
	if err := os.MkdirAll(extractPath, 0755); err != nil {
		return "", 0, fmt.Errorf("failed to create extraction directory: %w", err)
	}

	log.Info().Msgf("Extracting %s to %s...\n", targetKey, extractPath)

	// GNU Tar natively auto-detects the compression format (GZ, ZSTD, BZIP2, etc.)
	if err := runCmd("tar", "-xf", archivePath, "-C", extractPath); err != nil {
		return "", 0, fmt.Errorf("failed to extract downloaded payload: %w", err)
	}

	os.Remove(archivePath)

	return extractPath, downloadLen, nil
}
