package otlp

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrBodyTooLarge reports a request body over the limit, measured after decompression.
var ErrBodyTooLarge = errors.New("otlp: request body too large")

// ErrUnsupportedEncoding reports a Content-Encoding other than gzip or identity.
var ErrUnsupportedEncoding = errors.New("otlp: unsupported content encoding")

// ReadBody reads an export request body, decompressing it when contentEncoding is gzip. limit bounds the decompressed size, so a small compressed body cannot expand without bound; limit <= 0 means no bound.
func ReadBody(body io.Reader, contentEncoding string, limit int64) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return nil, fmt.Errorf("otlp: gzip: %w", err)
		}
		defer zr.Close()
		body = zr
	default:
		return nil, ErrUnsupportedEncoding
	}
	if limit <= 0 {
		return io.ReadAll(body)
	}
	b, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrBodyTooLarge
	}
	return b, nil
}
