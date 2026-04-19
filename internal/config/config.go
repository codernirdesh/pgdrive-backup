package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds all configuration for the backup pipeline.
type Config struct {
	// PostgreSQL
	PGHost     string
	PGPort     string
	PGUser     string
	PGPassword string
	PGDatabase string

	// Encryption
	EncryptionKey string // 32-byte hex key for AES-256

	// Google Drive
	GoogleDriveCredentials string // path to OAuth client credentials JSON
	GoogleDriveFolderID    string // target folder ID
	GoogleDriveTokenFile   string // path to cached OAuth token

	// Retention
	RetentionDays int

	// Backup naming
	BackupPrefix string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	retentionStr := getEnv("RETENTION_DAYS", "7")
	retentionDays, err := strconv.Atoi(retentionStr)
	if err != nil {
		return nil, fmt.Errorf("invalid RETENTION_DAYS: %w", err)
	}

	cfg := &Config{
		PGHost:                 requireEnv("PG_HOST"),
		PGPort:                 getEnv("PG_PORT", "5432"),
		PGUser:                 requireEnv("PG_USER"),
		PGPassword:             requireEnv("PG_PASSWORD"),
		PGDatabase:             requireEnv("PG_DATABASE"),
		EncryptionKey:          requireEnv("ENCRYPTION_KEY"),
		GoogleDriveCredentials: getEnv("GOOGLE_DRIVE_CREDENTIALS", "credentials.json"),
		GoogleDriveFolderID:    requireEnv("GOOGLE_DRIVE_FOLDER_ID"),
		GoogleDriveTokenFile:   getEnv("GOOGLE_DRIVE_TOKEN_FILE", "token.json"),
		RetentionDays:          retentionDays,
		BackupPrefix:           getEnv("BACKUP_PREFIX", "pgbackup"),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	required := map[string]string{
		"PG_HOST":                c.PGHost,
		"PG_USER":                c.PGUser,
		"PG_PASSWORD":            c.PGPassword,
		"PG_DATABASE":            c.PGDatabase,
		"ENCRYPTION_KEY":         c.EncryptionKey,
		"GOOGLE_DRIVE_FOLDER_ID": c.GoogleDriveFolderID,
	}
	for name, val := range required {
		if val == "" {
			return fmt.Errorf("required environment variable %s is not set", name)
		}
	}
	if len(c.EncryptionKey) != 64 {
		return fmt.Errorf("ENCRYPTION_KEY must be 64 hex characters (32 bytes for AES-256), got %d", len(c.EncryptionKey))
	}
	return nil
}

// PGConnString returns a PostgreSQL connection URI.
func (c *Config) PGConnString() string {
	return fmt.Sprintf("postgresql://%s:%s@%s:%s/%s",
		c.PGUser, c.PGPassword, c.PGHost, c.PGPort, c.PGDatabase)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func requireEnv(key string) string {
	return os.Getenv(key)
}
