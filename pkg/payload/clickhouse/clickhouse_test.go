package clickhouse

import (
	"errors"
	"log/slog"
)

var errFlush = errors.New("flush boom")

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
