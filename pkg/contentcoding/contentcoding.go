// Package contentcoding decodes HTTP entity bodies by their
// Content-Encoding. One concern: turning the compressed bytes a
// transparent forwarder captured into the plaintext every post-flight
// consumer (usage extraction, payload capture) needs.
//
// The pipeline runner never needs this — it strips the caller's
// Accept-Encoding so Go's transport hands it decompressed bodies. The
// proxy runner forwards Accept-Encoding verbatim (transparency is its
// contract), so its tee'd copy arrives however the upstream compressed
// it. Decoding belongs post-flight, off the hot path: the wire bytes
// stay untouched for the caller.
//
// Deliberately server-side (pkg/, not sdk/): brotli and zstd pull in
// third-party decoders the vendorable SDK must not carry. The SDK's
// extractors keep their stdlib gzip sniff for standalone use; server
// runners decode here first so the extractor input is already plain.
package contentcoding

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// maxDecodedBytes bounds a single decoded body. A hostile or broken
// upstream could otherwise expand a small captured body into gigabytes
// (zip-bomb) inside the post-flight goroutine.
const maxDecodedBytes = 512 << 20

// Decode returns body decoded per encoding (a Content-Encoding header
// value; comma-separated codings are applied in reverse order, per RFC
// 9110). An empty or identity encoding falls back to magic-byte
// sniffing for gzip/zstd — a forwarder that lost the header still gets
// its capture decoded. Unknown codings and corrupt data return an
// error; callers keep the raw bytes in that case.
func Decode(body []byte, encoding string) ([]byte, error) {
	codings := parseCodings(encoding)
	if len(codings) == 0 {
		if c := sniff(body); c != "" {
			codings = []string{c}
		} else {
			return body, nil
		}
	}
	var err error
	for i := len(codings) - 1; i >= 0; i-- {
		body, err = decodeOne(body, codings[i])
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

func parseCodings(encoding string) []string {
	var out []string
	for _, c := range strings.Split(encoding, ",") {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" || c == "identity" {
			continue
		}
		out = append(out, c)
	}
	return out
}

func sniff(body []byte) string {
	switch {
	case len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b:
		return "gzip"
	case len(body) >= 4 && body[0] == 0x28 && body[1] == 0xb5 && body[2] == 0x2f && body[3] == 0xfd:
		return "zstd"
	}
	return ""
}

func decodeOne(body []byte, coding string) ([]byte, error) {
	switch coding {
	case "gzip", "x-gzip":
		gr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("contentcoding: gzip: %w", err)
		}
		defer gr.Close()
		return readAll(gr, coding)
	case "deflate":
		// RFC says zlib-wrapped, but raw-deflate senders exist in the wild.
		zr, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			fr := flate.NewReader(bytes.NewReader(body))
			defer fr.Close()
			return readAll(fr, coding)
		}
		defer zr.Close()
		return readAll(zr, coding)
	case "br":
		return readAll(brotli.NewReader(bytes.NewReader(body)), coding)
	case "zstd":
		zr, err := zstd.NewReader(bytes.NewReader(body),
			zstd.WithDecoderMaxMemory(maxDecodedBytes))
		if err != nil {
			return nil, fmt.Errorf("contentcoding: zstd: %w", err)
		}
		defer zr.Close()
		return readAll(zr.IOReadCloser(), coding)
	default:
		return nil, fmt.Errorf("contentcoding: unsupported coding %q", coding)
	}
}

func readAll(r io.Reader, coding string) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(r, maxDecodedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("contentcoding: %s: %w", coding, err)
	}
	if len(out) > maxDecodedBytes {
		return nil, fmt.Errorf("contentcoding: %s: decoded body exceeds %d bytes", coding, maxDecodedBytes)
	}
	return out, nil
}
