package compressor

import (
	"compress/gzip"
	"io"
	"log"
)

// NewGzipReader wraps an input reader with maximum gzip compression.
// Returns an io.ReadCloser — closing it will flush/close the gzip writer.
func NewGzipReader(src io.Reader) io.ReadCloser {
	pr, pw := io.Pipe()

	go func() {
		gw, err := gzip.NewWriterLevel(pw, gzip.BestCompression)
		if err != nil {
			pw.CloseWithError(err)
			return
		}

		n, err := io.Copy(gw, src)
		if err != nil {
			_ = gw.Close()
			pw.CloseWithError(err)
			return
		}

		log.Printf("compressed %d bytes", n)

		if err := gw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}

		pw.Close()
	}()

	return pr
}
