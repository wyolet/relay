package contentcoding

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

var plain = []byte(`{"usage":{"input_tokens":8,"output_tokens":16}}`)

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zstded(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecode_PerCoding(t *testing.T) {
	var zlibBuf bytes.Buffer
	zw := zlib.NewWriter(&zlibBuf)
	_, _ = zw.Write(plain)
	_ = zw.Close()

	var rawFlateBuf bytes.Buffer
	fw, _ := flate.NewWriter(&rawFlateBuf, flate.DefaultCompression)
	_, _ = fw.Write(plain)
	_ = fw.Close()

	var brBuf bytes.Buffer
	bw := brotli.NewWriter(&brBuf)
	_, _ = bw.Write(plain)
	_ = bw.Close()

	cases := []struct {
		name     string
		body     []byte
		encoding string
	}{
		{"gzip", gzipped(t, plain), "gzip"},
		{"x-gzip", gzipped(t, plain), "x-gzip"},
		{"gzip uppercase", gzipped(t, plain), "GZIP"},
		{"deflate zlib-wrapped", zlibBuf.Bytes(), "deflate"},
		{"deflate raw", rawFlateBuf.Bytes(), "deflate"},
		{"br", brBuf.Bytes(), "br"},
		{"zstd", zstded(t, plain), "zstd"},
		{"identity", plain, "identity"},
		{"empty encoding plain body", plain, ""},
		{"sniffed gzip no header", gzipped(t, plain), ""},
		{"sniffed zstd no header", zstded(t, plain), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decode(tc.body, tc.encoding)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !bytes.Equal(got, plain) {
				t.Fatalf("Decode: got %q, want %q", got, plain)
			}
		})
	}
}

func TestDecode_CommaChainAppliedInReverse(t *testing.T) {
	// "gzip, zstd" means gzip applied first, zstd second → undo zstd, then gzip.
	body := zstded(t, gzipped(t, plain))
	got, err := Decode(body, "gzip, zstd")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("Decode: got %q, want %q", got, plain)
	}
}

func TestDecode_Errors(t *testing.T) {
	if _, err := Decode(plain, "compress"); err == nil {
		t.Fatal("unsupported coding: want error")
	}
	if _, err := Decode([]byte("\x1f\x8bnot really gzip"), "gzip"); err == nil {
		t.Fatal("corrupt gzip: want error")
	}
}
