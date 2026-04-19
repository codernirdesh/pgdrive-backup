.PHONY: build run decrypt clean docker keygen

# Load .env if it exists
ifneq (,$(wildcard .env))
  include .env
  export
endif

# Build both binaries
build:
	go build -o bin/backup  ./cmd/backup
	go build -o bin/decrypt ./cmd/decrypt

# Run backup (loads .env automatically)
run: build
	./bin/backup

# Decrypt a backup file
# Usage: make decrypt KEY=<hex-key> INPUT=<file.sql.gz.enc> OUTPUT=<file.sql>
decrypt: build
	./bin/decrypt -key $(KEY) -input $(INPUT) -output $(OUTPUT)

# Build Docker image
docker:
	docker build -t pgdrive-backup .

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
