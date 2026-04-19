package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/codernirdesh/pgdrive-backup/internal/config"
	"github.com/codernirdesh/pgdrive-backup/internal/pipeline"
)

func main() {
	log.SetFlags(log.Ltime)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := pipeline.Run(ctx, cfg); err != nil {
		log.Fatalf("Backup failed: %v", err)
	}
}
