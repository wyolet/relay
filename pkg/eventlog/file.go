package eventlog

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
)

type fileSink struct {
	cfg    Config
	logger *Logger

	f    *os.File
	bw   *bufio.Writer
	date string
}

func newFileSink(cfg Config) (*fileSink, error) {
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("eventlog: mkdir %s: %w", cfg.Dir, err)
	}
	return &fileSink{cfg: cfg}, nil
}

func (fs *fileSink) setLogger(l *Logger) { fs.logger = l }

func (fs *fileSink) write(b []byte) error {
	now := fs.cfg.Clock().UTC()
	d := now.Format("2006-01-02")
	if d != fs.date {
		fs.closeFile()
		if err := fs.openFile(d); err != nil {
			return err
		}
	}
	fs.bw.Write(b)
	fs.bw.WriteByte('\n')
	return nil
}

func (fs *fileSink) flush() {
	if fs.bw != nil {
		fs.bw.Flush()
	}
}

func (fs *fileSink) ping(_ context.Context) error { return nil }

func (fs *fileSink) close(_ context.Context) error {
	fs.closeFile()
	return nil
}

func (fs *fileSink) openFile(d string) error {
	name := filepath.Join(fs.cfg.Dir, "events-"+d+".jsonl")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fs.f = f
	fs.bw = bufio.NewWriter(f)
	fs.date = d
	if fs.logger != nil {
		fs.logger.setCurrentFile(name)
	}
	return nil
}

func (fs *fileSink) closeFile() {
	if fs.bw != nil {
		fs.bw.Flush()
	}
	if fs.f != nil {
		fs.f.Sync()
		fs.f.Close()
		fs.f = nil
		fs.bw = nil
	}
}
