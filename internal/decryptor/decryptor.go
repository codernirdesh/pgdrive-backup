package decryptor

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"io"
	"log"
)

// NewDecryptingReader wraps a source reader with AES-256-CTR decryption.
// Expects the first 16 bytes to be the IV (as written by the encryptor).
func NewDecryptingReader(src io.Reader, hexKey string) (io.ReadCloser, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("decode encryption key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}

	// Read the IV from the first 16 bytes
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(src, iv); err != nil {
		return nil, fmt.Errorf("read IV: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}

	stream := cipher.NewCTR(block, iv)

	pr, pw := io.Pipe()

	go func() {
		log.Println("[decryptor] AES-256-CTR decryption stream started")

		buf := make([]byte, 32*1024)
		var total int64
		for {
			n, readErr := src.Read(buf)
			if n > 0 {
				total += int64(n)
				decrypted := make([]byte, n)
				stream.XORKeyStream(decrypted, buf[:n])
				if _, writeErr := pw.Write(decrypted); writeErr != nil {
					pw.CloseWithError(writeErr)
					return
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				pw.CloseWithError(readErr)
				return
			}
		}

		log.Printf("[decryptor] decrypted %d bytes", total)
		pw.Close()
	}()

	return pr, nil
}
