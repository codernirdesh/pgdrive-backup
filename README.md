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
- **Backup browser** — list, sort, download, and decrypt Drive backups interactively
- **Web restore UI** — serve backups in a browser with one-click download/decrypt actions
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

### 3b. Serve the web UI

**Local:**

```bash
make serve-web
```

Then open `http://localhost:8080`.

**Docker:**

```bash
make docker-web
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

### Option A: Browse backups directly from Google Drive

```bash
./bin/decrypt
```

That opens an interactive picker which:

- lists all backups in the configured Drive folder
- sorts them newest first
- lets you download the encrypted backup, decrypt it, or do both

You can also just list them:

```bash
make list-backups
# or
./bin/decrypt -list
```

### Option A2: Use the web UI

```bash
./bin/web
```

The web UI:

- shows all Drive backups sorted newest first
- lets you filter by filename in the browser
- downloads the raw encrypted `.enc` backup with one click
- downloads a decrypted `.dump` restore archive when `ENCRYPTION_KEY` is set

> Web mode expects an existing Drive token file. If `token.json` is missing, authorize once with `./bin/decrypt -list` first.

### Option B: Download one manually, then decrypt locally

```bash
make decrypt KEY=<your-hex-key> INPUT=backup.sql.gz.enc OUTPUT=backup.dump

# Or directly:
./bin/decrypt -key <hex-key> -input backup.sql.gz.enc -output backup.dump
```

The decrypted output is a **pg_dump custom-format archive** suitable for `pg_restore`.

### 3. Restore to PostgreSQL

```bash
pg_restore -h <host> -U <user> -d <database> backup.dump
```

### Handy non-interactive examples

```bash
# decrypt the newest backup straight from Drive
./bin/decrypt -latest -action decrypt -output ./restore.dump

# download only the second newest encrypted backup
./bin/decrypt -select 2 -action download -download ./downloads

# save both the encrypted file and the decrypted restore archive
./bin/decrypt -latest -action both -download ./downloads -output ./restore.dump
```

## Project Structure

```
.
├── cmd/
│   ├── backup/       # Main backup CLI entry point
│   ├── decrypt/      # Decrypt + decompress utility
│   └── web/          # Browser-based restore UI
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

> The `.sql.gz.enc` suffix is historical; after decrypting and decompressing, the payload is actually a `pg_dump --format=custom` archive, so restore it with `pg_restore`.

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
