package dumper

import (
	"fmt"
	"io"
	"log"
	"os/exec"

	"github.com/codernirdesh/pgdrive-backup/internal/config"
)

// Stream starts pg_dump and returns an io.ReadCloser that streams the SQL dump.
// The caller is responsible for closing the reader.
// Uses --no-owner and --clean for portable restores.
func Stream(cfg *config.Config) (io.ReadCloser, func() error, error) {
	args := []string{
		"--host", cfg.PGHost,
		"--port", cfg.PGPort,
		"--username", cfg.PGUser,
		"--dbname", cfg.PGDatabase,
		"--no-owner",
		"--clean",
		"--if-exists",
		"--format=custom",
		"--compress=0", // we compress ourselves downstream
	}

	cmd := exec.Command("pg_dump", args...)
	cmd.Env = append(cmd.Environ(), fmt.Sprintf("PGPASSWORD=%s", cfg.PGPassword))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("pg_dump stdout pipe: %w", err)
	}

	cmd.Stderr = &logWriter{prefix: "pg_dump"}

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("pg_dump start: %w", err)
	}

	// wait function to call after the reader is drained
	wait := func() error {
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("pg_dump exited with error: %w", err)
		}

		return nil
	}

	return stdout, wait, nil
}

// logWriter writes each line from pg_dump stderr to the Go logger.
type logWriter struct {
	prefix string
}

func (w *logWriter) Write(p []byte) (int, error) {
	log.Printf("%s: %s", w.prefix, string(p))
	return len(p), nil
}
