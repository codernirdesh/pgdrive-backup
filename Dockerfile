# ── Build ────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /bin/backup   ./cmd/backup
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /bin/decrypt   ./cmd/decrypt
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /bin/web       ./cmd/web

# ── Runtime ──────────────────────────────────────────────────
FROM alpine:3.21

RUN apk add --no-cache postgresql16-client ca-certificates tzdata

COPY --from=builder /bin/backup  /usr/local/bin/backup
COPY --from=builder /bin/decrypt /usr/local/bin/decrypt
COPY --from=builder /bin/web     /usr/local/bin/web

EXPOSE 8080

# Default: run backup
ENTRYPOINT ["backup"]
