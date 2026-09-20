package clickhouse

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/pkg/payload"
)

// Get returns the captured record (bodies included) for one request id. The
// newest row wins if an id was somehow reused. Returns payload.ErrNotFound
// when absent.
func (s *Reader) Get(ctx context.Context, requestID string) (payload.Record, error) {
	sql := fmt.Sprintf(
		"SELECT request_id, ts, request_body, response_body, request_truncated, response_truncated FROM %s WHERE request_id = ? ORDER BY ts DESC LIMIT 1",
		chTable)

	rows, err := s.conn.Query(ctx, sql, requestID)
	if err != nil {
		return payload.Record{}, fmt.Errorf("payload/clickhouse: get query: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return payload.Record{}, err
		}
		return payload.Record{}, payload.ErrNotFound
	}

	var (
		r                   payload.Record
		reqTrunc, respTrunc uint8
		reqBody, respBody   string
	)
	if err := rows.Scan(
		&r.RequestID, &r.Timestamp, &reqBody, &respBody, &reqTrunc, &respTrunc,
	); err != nil {
		return payload.Record{}, fmt.Errorf("payload/clickhouse: scan get row: %w", err)
	}
	r.RequestTruncated = reqTrunc == 1
	r.ResponseTruncated = respTrunc == 1
	if reqBody != "" {
		r.RequestBody = []byte(reqBody)
	}
	if respBody != "" {
		r.ResponseBody = []byte(respBody)
	}
	return r, rows.Err()
}
