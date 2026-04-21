package pipeline

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/codernirdesh/pgdrive-backup/internal/compressor"
	"github.com/codernirdesh/pgdrive-backup/internal/config"
	"github.com/codernirdesh/pgdrive-backup/internal/dumper"
	"github.com/codernirdesh/pgdrive-backup/internal/encryptor"
	"github.com/codernirdesh/pgdrive-backup/internal/uploader"
)

// progressReader wraps an io.Reader and periodically calls emit with bytes read so far.
type progressReader struct {
	r        io.Reader
	total    int64
	last     time.Time
	interval time.Duration
	label    string
	emit     func(string, ...any)
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	p.total += int64(n)
	if time.Since(p.last) >= p.interval {
		p.emit("%s — %s", p.label, fmtBytes(p.total))
		p.last = time.Now()
	}
	return n, err
}

func fmtBytes(b int64) string {
	const k, m, g = 1024, 1024 * 1024, 1024 * 1024 * 1024
	switch {
	case b >= g:
		return fmt.Sprintf("%.2f GB", float64(b)/g)
	case b >= m:
		return fmt.Sprintf("%.1f MB", float64(b)/m)
	case b >= k:
		return fmt.Sprintf("%.1f KB", float64(b)/k)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// Run executes the full backup pipeline: dump → compress → encrypt → upload → cleanup.
func Run(ctx context.Context, cfg *config.Config) error {
	return RunWithLogger(ctx, cfg, nil)
}

// RunWithLogger executes the full backup pipeline, calling logFn for each progress message.
// If logFn is nil the default log package is used.
func RunWithLogger(ctx context.Context, cfg *config.Config, logFn func(string)) error {
	emit := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		log.Print(msg)
		if logFn != nil {
			logFn(msg)
		}
	}

	start := time.Now()
	emit("── backup started ──")

	emit("dumping %s...", cfg.PGDatabase)
	dumpReader, waitDump, err := dumper.Stream(cfg)
	if err != nil {
		return fmt.Errorf("start dump: %w", err)
	}

	dumpProgress := &progressReader{
		r:        dumpReader,
		interval: 2 * time.Second,
		last:     time.Now(),
		label:    "dump progress",
		emit:     emit,
	}

	compressedReader := compressor.NewGzipReader(dumpProgress)
	defer compressedReader.Close()

	encryptedReader, err := encryptor.NewEncryptingReader(compressedReader, cfg.EncryptionKey)
	if err != nil {
		return fmt.Errorf("create encryptor: %w", err)
	}
	defer encryptedReader.Close()

	emit("connecting to Google Drive...")
	driveClient, err := uploader.New(ctx, cfg.GoogleDriveCredentials, cfg.GoogleDriveFolderID, cfg.GoogleDriveTokenFile)
	if err != nil {
		return fmt.Errorf("create drive client: %w", err)
	}

	filename := generateFilename(cfg.BackupPrefix, cfg.PGDatabase)
	emit("uploading %s...", filename)

	uploadProgress := &progressReader{
		r:        encryptedReader,
		interval: 2 * time.Second,
		last:     time.Now(),
		label:    "upload progress",
		emit:     emit,
	}

	_, err = driveClient.Upload(ctx, filename, uploadProgress)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	emit("uploaded %s (%s)", filename, fmtBytes(uploadProgress.total))

	if err := waitDump(); err != nil {
		return fmt.Errorf("dump wait: %w", err)
	}

	maxAge := time.Duration(cfg.RetentionDays) * 24 * time.Hour
	deleted, err := driveClient.DeleteOlderThan(ctx, cfg.BackupPrefix, maxAge)
	if err != nil {
		emit("cleanup error (non-fatal): %v", err)
	} else if deleted > 0 {
		emit("cleanup: %d old backup(s) removed", deleted)
	}

	emit("── completed in %s ──", time.Since(start).Round(time.Second))
	return nil
}

func generateFilename(prefix, database string) string {
	ts := time.Now().UTC().Format("2006-01-02_150405")
	return fmt.Sprintf("%s_%s_%s.sql.gz.enc", prefix, database, ts)
}
