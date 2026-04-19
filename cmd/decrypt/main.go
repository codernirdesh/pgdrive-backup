package main

import (
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/codernirdesh/pgdrive-backup/internal/decryptor"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[decrypt] ")

	key := flag.String("key", "", "64-char hex encryption key (AES-256)")
	input := flag.String("input", "", "path to encrypted .sql.gz.enc file")
	output := flag.String("output", "", "path for decrypted output .sql file (default: stdout)")
	flag.Parse()

	if *key == "" || *input == "" {
		fmt.Fprintln(os.Stderr, "Usage: decrypt -key <hex-key> -input <file.sql.gz.enc> [-output <file.sql>]")
		os.Exit(1)
	}

	f, err := os.Open(*input)
	if err != nil {
		log.Fatalf("open input: %v", err)
	}
	defer f.Close()

	// Decrypt
	decryptedReader, err := decryptor.NewDecryptingReader(f, *key)
	if err != nil {
		log.Fatalf("create decryptor: %v", err)
	}
	defer decryptedReader.Close()

	// Decompress
	gzReader, err := gzip.NewReader(decryptedReader)
	if err != nil {
		log.Fatalf("create gzip reader: %v", err)
	}
	defer gzReader.Close()

	// Write output
	var w io.Writer = os.Stdout
	if *output != "" {
		outFile, err := os.Create(*output)
		if err != nil {
			log.Fatalf("create output file: %v", err)
		}
		defer outFile.Close()
		w = outFile
	}

	n, err := io.Copy(w, gzReader)
	if err != nil {
		log.Fatalf("write output: %v", err)
	}

	log.Printf("Decrypted and decompressed %d bytes", n)
}
