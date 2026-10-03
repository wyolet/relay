package wal

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// ActiveName is the file records are appended to before rotation.
	ActiveName = "active.jsonl"
	// SegmentGlob matches the rotated, not-yet-flushed segments.
	SegmentGlob = "segment-*.jsonl"

	segmentPrefix = "segment-"

	// scanBuf bounds a single WAL line. Keep this high enough for the
	// largest record a sink may write (payload bodies are base64 in JSON,
	// with a ~32 MB provider request ceiling) while still treating
	// pathological lines as droppable.
	scanBuf = 48 << 20
)

// readSegment decodes one segment file. Oversized and undecodable lines are
// logged and skipped rather than failing the whole segment — one bad record
// must not block every later one from reaching the sink.
func readSegment[T any](path string, log *slog.Logger) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var records []T
	r := bufio.NewReader(f)
	lineNo := 0
	for {
		line, tooLong, err := readLine(r)
		if err != nil {
			if err == io.EOF {
				return records, nil
			}
			return records, err
		}
		lineNo++
		if tooLong {
			log.Warn("wal: skip oversized line", "file", filepath.Base(path), "line", lineNo)
			continue
		}
		if len(line) == 0 {
			continue
		}
		var rec T
		if err := json.Unmarshal(line, &rec); err != nil {
			log.Warn("wal: skip undecodable line", "file", filepath.Base(path), "line", lineNo, "err", err)
			continue
		}
		records = append(records, rec)
	}
}

// CountLines returns the number of records in a WAL file. A missing file
// counts as zero.
func CountLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	n := 0
	r := bufio.NewReader(f)
	for {
		line, tooLong, err := readLine(r)
		if err != nil {
			if err == io.EOF {
				return n, nil
			}
			return n, err
		}
		if tooLong || len(line) > 0 {
			n++
		}
	}
}

// readLine reads one newline-terminated record, reporting tooLong (and
// discarding the line) when it exceeds scanBuf.
func readLine(r *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		if len(line)+len(frag) > scanBuf {
			if err == bufio.ErrBufferFull {
				for err == bufio.ErrBufferFull {
					_, err = r.ReadSlice('\n')
				}
			}
			if err != nil && err != io.EOF {
				return nil, false, err
			}
			return nil, true, nil
		}
		line = append(line, frag...)

		switch err {
		case nil:
			return trimLine(line), false, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(line) == 0 {
				return nil, false, io.EOF
			}
			return trimLine(line), false, nil
		default:
			return nil, false, err
		}
	}
}

func trimLine(line []byte) []byte {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line
}

// segmentNano extracts the unix-nano suffix from a segment filename for
// stable chronological sort. Returns 0 on parse failure.
func segmentNano(path string) int {
	base := filepath.Base(path)
	s := strings.TrimPrefix(base, segmentPrefix)
	s = strings.TrimSuffix(s, ".jsonl")
	n, _ := strconv.Atoi(s)
	return n
}
