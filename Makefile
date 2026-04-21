.PHONY: build run decrypt browse list-backups serve-web clean docker docker-web keygen

# Load .env if it exists
ifneq (,$(wildcard .env))
  include .env
  export
endif

# Build both binaries
build:
	go build -o bin/backup  ./cmd/backup
	go build -o bin/decrypt ./cmd/decrypt
	go build -o bin/web     ./cmd/web

# Run backup (loads .env automatically)
run: build
	./bin/backup

# Decrypt a backup file
# Usage: make decrypt KEY=<hex-key> INPUT=<file.sql.gz.enc> OUTPUT=<file.sql>
decrypt: build
	./bin/decrypt -key $(KEY) -input $(INPUT) -output $(OUTPUT)

# Browse Google Drive backups interactively
browse: build
	./bin/decrypt

# List backups currently stored in Google Drive
list-backups: build
	./bin/decrypt -list

# Start the browser-based backup UI
serve-web: build
	./bin/web

# Build Docker image
docker:
	docker build -t pgdrive-backup .

# Run the web UI via Docker
docker-web: docker
	docker run --rm -p 8080:8080 --env-file .env \
		-v $(PWD)/credentials.json:/app/credentials.json:ro \
		-v $(PWD)/token.json:/app/token.json \
		-e GOOGLE_DRIVE_CREDENTIALS=/app/credentials.json \
		-e GOOGLE_DRIVE_TOKEN_FILE=/app/token.json \
		-e WEB_ADDR=:8080 \
		--entrypoint /usr/local/bin/web \
		pgdrive-backup

# Run via Docker
docker-run:
	docker run --rm --env-file .env \
		-v $(PWD)/credentials.json:/app/credentials.json:ro \
		-e GOOGLE_DRIVE_CREDENTIALS=/app/credentials.json \
		pgdrive-backup

# Generate a new AES-256 encryption key
keygen:
	@openssl rand -hex 32

# Clean build artifacts
clean:
	rm -rf bin/
