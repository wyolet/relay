package clickhouse

import (
	"errors"
	"io"
	"log/slog"
)

var errFlush = errors.New("flush boom")

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
