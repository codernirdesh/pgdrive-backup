package encryptor

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
)

// NewEncryptingReader wraps a source reader with AES-256-CTR encryption.
// The first 16 bytes of the output are the random IV.
// hexKey must be 64 hex characters (32 bytes).
func NewEncryptingReader(src io.Reader, hexKey string) (io.ReadCloser, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("decode encryption key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}

	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, fmt.Errorf("generate IV: %w", err)
	}

	stream := cipher.NewCTR(block, iv)

	pr, pw := io.Pipe()

	go func() {
		// Write IV as the first 16 bytes
		if _, err := pw.Write(iv); err != nil {
			pw.CloseWithError(err)
			return
		}

		buf := make([]byte, 32*1024) // 32KB buffer
		var totalIn int64
		for {
			n, readErr := src.Read(buf)
			if n > 0 {
				totalIn += int64(n)
				encrypted := make([]byte, n)
				stream.XORKeyStream(encrypted, buf[:n])
				if _, writeErr := pw.Write(encrypted); writeErr != nil {
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

		log.Printf("encrypted %d bytes", totalIn)
		pw.Close()
	}()

	return pr, nil
}
