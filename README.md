# pgdrive-backup

Zero-downtime streaming backup pipeline that dumps a PostgreSQL database, compresses (gzip max), encrypts (AES-256-CTR), and uploads directly to Google Drive — all without touching disk. Old backups are automatically purged by retention policy.

## Architecture

```
pg_dump ──→ gzip (BestCompression) ──→ AES-256-CTR ──→ Google Drive
  │                  │                       │                │
  └─ streams stdout  └─ io.Pipe()            └─ io.Pipe()     └─ resumable upload
```

All stages are connected via `io.Pipe()` — **zero temp files, constant memory**.

## Features

- **Zero downtime** — uses `pg_dump` (no table locks for reads)
- **No temporary files** — true streaming from dump to cloud
- **Constant memory** — 32KB buffer per stage
- **End-to-end encryption** — AES-256-CTR with random IV per backup
- **Max compression** — gzip level 9 (BestCompression)
- **Automated retention** — deletes backups older than N days
- **Decrypt utility** — included CLI to decrypt + decompress backups
- **Docker ready** — multi-stage Alpine image with `pg_dump` included

## Quick Start

### 1. Google Drive Setup

1. Go to [Google Cloud Console](https://console.cloud.google.com/)
2. Create a project (or select existing)
3. Enable the **Google Drive API**
4. Create a **Service Account** → download the JSON key as `credentials.json`
5. Create a folder in Google Drive
6. **Share that folder** with the service account email (found in `credentials.json` → `client_email`)
7. Copy the folder ID from the URL: `https://drive.google.com/drive/folders/<FOLDER_ID>`

### 2. Configuration

```bash
cp .env.example .env
```

Edit `.env` with your values:

```env
PG_HOST=your-host.hostinger.com
PG_PORT=5432
PG_USER=your_user
PG_PASSWORD=your_password
PG_DATABASE=your_database
ENCRYPTION_KEY=<64-hex-chars>
GOOGLE_DRIVE_CREDENTIALS=credentials.json
GOOGLE_DRIVE_FOLDER_ID=<folder-id>
RETENTION_DAYS=7
BACKUP_PREFIX=pgbackup
```

Generate an encryption key:

```bash
make keygen
# or: openssl rand -hex 32
```

### 3. Run

**Local (requires `pg_dump` installed):**

```bash
# Source env vars
export $(grep -v '^#' .env | xargs)

make run
```

**Docker:**

```bash
make docker
make docker-run
```

### 4. Automate with Cron

```bash
# Daily at 2 AM UTC
0 2 * * * cd /path/to/pgdrive-backup && export $(grep -v '^#' .env | xargs) && ./bin/backup >> /var/log/pgbackup.log 2>&1
```

Or with Docker:

```bash
0 2 * * * docker run --rm --env-file /path/to/.env -v /path/to/credentials.json:/app/credentials.json:ro -e GOOGLE_DRIVE_CREDENTIALS=/app/credentials.json pgdrive-backup >> /var/log/pgbackup.log 2>&1
```

## Restoring a Backup

### 1. Download the `.sql.gz.enc` file from Google Drive

### 2. Decrypt and decompress

```bash
make decrypt KEY=<your-hex-key> INPUT=backup.sql.gz.enc OUTPUT=backup.sql

# Or directly:
./bin/decrypt -key <hex-key> -input backup.sql.gz.enc -output backup.sql
```

### 3. Restore to PostgreSQL

```bash
pg_restore -h <host> -U <user> -d <database> backup.sql
```

## Project Structure

```
.
├── cmd/
│   ├── backup/       # Main backup CLI entry point
│   └── decrypt/      # Decrypt + decompress utility
├── internal/
│   ├── config/       # Environment-based configuration
│   ├── dumper/       # pg_dump streaming wrapper
│   ├── compressor/   # gzip max-compression stream
│   ├── encryptor/    # AES-256-CTR encrypting stream
│   ├── decryptor/    # AES-256-CTR decrypting stream
│   ├── uploader/     # Google Drive upload + retention
│   └── pipeline/     # Orchestrates the full pipeline
├── Dockerfile        # Multi-stage Alpine build
├── Makefile          # Build, run, docker, keygen
├── .env.example      # Template configuration
└── README.md
```

## Backup File Naming

Files are named: `{prefix}_{database}_{UTC-timestamp}.sql.gz.enc`

Example: `pgbackup_mydb_2026-04-19_020000.sql.gz.enc`

## Security Notes

- **Encryption key**: Store your 64-char hex key securely (password manager, vault). Losing it means backups are unrecoverable.
- **Service account JSON**: Keep `credentials.json` out of version control (it's in `.gitignore`).
- **AES-256-CTR**: Each backup gets a unique random 16-byte IV prepended to the ciphertext.
- **PGPASSWORD**: Passed via environment variable to `pg_dump` (standard PostgreSQL practice).

## Requirements

- Go 1.22+ (build)
- `pg_dump` (runtime, or use Docker)
- Google Cloud service account with Drive API access

## License

MIT
