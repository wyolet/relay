package seed

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// rawUSTARBlock packs one 512-byte ustar header by hand; archive/tar's Writer
// drops GNU.sparse.* records, so sparse entries cannot go through it.
func rawUSTARBlock(name string, size int64, typeflag byte) []byte {
	blk := make([]byte, 512)
	copy(blk[0:100], name)
	copy(blk[100:108], "0000644\x00")
	copy(blk[108:116], "0000000\x00")
	copy(blk[116:124], "0000000\x00")
	copy(blk[124:136], strconv.FormatInt(size, 8)+"\x00")
	copy(blk[136:148], "00000000000\x00")
	copy(blk[148:156], "        ")
	blk[156] = typeflag
	copy(blk[257:263], "ustar\x00")
	copy(blk[263:265], "00")
	var sum int
	for _, b := range blk {
		sum += int(b)
	}
	copy(blk[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return blk
}

func paxRecord(k, v string) string {
	body := " " + k + "=" + v + "\n"
	n := len(body)
	for digits := len(strconv.Itoa(n)); ; digits++ {
		if len(strconv.Itoa(n+digits)) == digits {
			return strconv.Itoa(n+digits) + body
		}
	}
}

// writeSparseEntry emits a PAX 0.1 sparse entry whose whole logical size is
// one hole: realSize bytes on disk from no data in the stream.
func writeSparseEntry(w *bytes.Buffer, name string, realSize int64) {
	text := paxRecord("GNU.sparse.major", "0") + paxRecord("GNU.sparse.minor", "1") +
		paxRecord("GNU.sparse.numblocks", "0") + paxRecord("GNU.sparse.map", "") +
		paxRecord("GNU.sparse.size", strconv.FormatInt(realSize, 10))
	w.Write(rawUSTARBlock("PaxHeader/"+name, int64(len(text)), tar.TypeXHeader))
	w.Write([]byte(text))
	if pad := (512 - len(text)%512) % 512; pad > 0 {
		w.Write(make([]byte, pad))
	}
	w.Write(rawUSTARBlock(name, 0, tar.TypeReg))
}

// gzipTar builds a gzipped tar from a writer callback; raw lets the
// callback append hand-packed blocks after flushing tw. The tar is not
// closed, so a truncated stream is part of the fixture.
func gzipTar(t *testing.T, build func(tw *tar.Writer, raw *bytes.Buffer)) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	build(tw, &raw)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeEntry(t *testing.T, tw *tar.Writer, hdr *tar.Header, data []byte) {
	t.Helper()
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader %s: %v", hdr.Name, err)
	}
	if len(data) > 0 {
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("Write %s: %v", hdr.Name, err)
		}
	}
	if err := tw.Flush(); err != nil && !strings.Contains(err.Error(), "missed writing") {
		t.Fatalf("Flush %s: %v", hdr.Name, err)
	}
}

func diskBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

func TestExtractTarGz_RejectsDeclaredSizeOverCap(t *testing.T) {
	archive := gzipTar(t, func(tw *tar.Writer, raw *bytes.Buffer) {
		writeEntry(t, tw, &tar.Header{Name: "a.yaml", Mode: 0o644, Typeflag: tar.TypeReg, Size: 1}, []byte("x"))
		writeSparseEntry(raw, "big.bin", int64(maxCatalogBytes)+1)
	})
	dir := t.TempDir()
	err := extractTarGz(bytes.NewReader(archive), dir, maxCatalogBytes, maxCatalogEntries)
	if err == nil || !strings.Contains(err.Error(), "archive exceeds") {
		t.Fatalf("err = %v, want size cap error", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "big.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("oversize entry was created (stat err=%v)", statErr)
	}
}

// A declared size near MaxInt64 after a small entry must not wrap the
// running total and slip past the cap.
func TestExtractTarGz_RejectsSizeThatWouldOverflowTotal(t *testing.T) {
	archive := gzipTar(t, func(tw *tar.Writer, _ *bytes.Buffer) {
		writeEntry(t, tw, &tar.Header{Name: "a.yaml", Mode: 0o644, Typeflag: tar.TypeReg, Size: 1}, []byte("x"))
		writeEntry(t, tw, &tar.Header{Name: "huge.bin", Mode: 0o644, Typeflag: tar.TypeReg, Size: math.MaxInt64, Format: tar.FormatPAX}, bytes.Repeat([]byte("Z"), 2048))
	})
	dir := t.TempDir()
	err := extractTarGz(bytes.NewReader(archive), dir, maxCatalogBytes, maxCatalogEntries)
	if err == nil || !strings.Contains(err.Error(), "archive exceeds") {
		t.Fatalf("err = %v, want size cap error", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "huge.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("overflowing entry was created (stat err=%v)", statErr)
	}
}

// Sparse holes cost no archive bytes but land on disk as zeros; they count
// against the cap at their logical size.
func TestExtractTarGz_SparseHolesCountAgainstCap(t *testing.T) {
	const capBytes = 1 << 20
	archive := gzipTar(t, func(_ *tar.Writer, raw *bytes.Buffer) {
		for i := range 4 {
			writeSparseEntry(raw, fmt.Sprintf("hole%d.bin", i), capBytes/2)
		}
		raw.Write(make([]byte, 1024))
	})
	dir := t.TempDir()
	err := extractTarGz(bytes.NewReader(archive), dir, capBytes, maxCatalogEntries)
	if err == nil || !strings.Contains(err.Error(), "archive exceeds") {
		t.Fatalf("err = %v, want size cap error", err)
	}
	if got := diskBytes(t, dir); got > capBytes {
		t.Fatalf("wrote %d bytes, cap is %d", got, capBytes)
	}
}

func TestExtractTarGz_RejectsTooManyEntries(t *testing.T) {
	archive := gzipTar(t, func(tw *tar.Writer, _ *bytes.Buffer) {
		for i := range 11 {
			writeEntry(t, tw, &tar.Header{Name: fmt.Sprintf("f%d.yaml", i), Mode: 0o644, Typeflag: tar.TypeReg}, nil)
		}
	})
	err := extractTarGz(bytes.NewReader(archive), t.TempDir(), maxCatalogBytes, 10)
	if err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("err = %v, want entry cap error", err)
	}
}
