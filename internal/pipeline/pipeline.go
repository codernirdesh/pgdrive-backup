package pipeline

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/codernirdesh/pgdrive-backup/internal/compressor"
	"github.com/codernirdesh/pgdrive-backup/internal/config"
	"github.com/codernirdesh/pgdrive-backup/internal/dumper"
	"github.com/codernirdesh/pgdrive-backup/internal/encryptor"
	"github.com/codernirdesh/pgdrive-backup/internal/uploader"
)

// Run executes the full backup pipeline: dump → compress → encrypt → upload → cleanup.
func Run(ctx context.Context, cfg *config.Config) error {
	start := time.Now()
	log.Println("── backup started ──")

	log.Printf("dumping %s...", cfg.PGDatabase)
	dumpReader, waitDump, err := dumper.Stream(cfg)
	if err != nil {
		return fmt.Errorf("start dump: %w", err)
	}

	compressedReader := compressor.NewGzipReader(dumpReader)
	defer compressedReader.Close()

	encryptedReader, err := encryptor.NewEncryptingReader(compressedReader, cfg.EncryptionKey)
	if err != nil {
		return fmt.Errorf("create encryptor: %w", err)
	}
	defer encryptedReader.Close()

	driveClient, err := uploader.New(ctx, cfg.GoogleDriveCredentials, cfg.GoogleDriveFolderID, cfg.GoogleDriveTokenFile)
	if err != nil {
		return fmt.Errorf("create drive client: %w", err)
	}

	filename := generateFilename(cfg.BackupPrefix, cfg.PGDatabase)
	_, err = driveClient.Upload(ctx, filename, encryptedReader)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}

	// Wait for pg_dump to finish cleanly
	if err := waitDump(); err != nil {
		return fmt.Errorf("dump wait: %w", err)
	}

	maxAge := time.Duration(cfg.RetentionDays) * 24 * time.Hour
	deleted, err := driveClient.DeleteOlderThan(ctx, cfg.BackupPrefix, maxAge)
	if err != nil {
		log.Printf("cleanup error (non-fatal): %v", err)
	} else if deleted > 0 {
		log.Printf("cleanup: %d old backup(s) removed", deleted)
	}

	elapsed := time.Since(start)
	log.Printf("── completed in %s ──", elapsed.Round(time.Second))

	return nil
}

func generateFilename(prefix, database string) string {
	ts := time.Now().UTC().Format("2006-01-02_150405")
	return fmt.Sprintf("%s_%s_%s.sql.gz.enc", prefix, database, ts)
}
