package clickhouse

import (
	"context"
	"errors"
	"fmt"

	"github.com/wyolet/relay/pkg/payload"
	"github.com/wyolet/relay/pkg/payload/dedup"
)

// Get returns the captured record (bodies included) for one request id. The
// newest row wins if an id was somehow reused. Rows written in dedup mode
// are tried first, then whole-body rows, so switching modes loses no
// history. Returns payload.ErrNotFound when absent from both.
func (s *Reader) Get(ctx context.Context, requestID string) (payload.Record, error) {
	r, err := s.getDedup(ctx, requestID)
	if !errors.Is(err, payload.ErrNotFound) {
		return r, err
	}
	return s.getWhole(ctx, requestID)
}

func (s *Reader) getWhole(ctx context.Context, requestID string) (payload.Record, error) {
	sql := fmt.Sprintf(
		"SELECT request_id, ts, request_body, response_body, request_truncated, response_truncated, project_id, principal_id, relay_key_hash FROM %s WHERE request_id = ? ORDER BY ts DESC LIMIT 1",
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
		&r.ProjectID, &r.PrincipalID, &r.RelayKeyHash,
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

// getDedup reads a payload_requests row and rebuilds its request body from
// payload_pieces. A missing piece or a body that does not match the row's
// digest is an error, never a partial or altered body.
func (s *Reader) getDedup(ctx context.Context, requestID string) (payload.Record, error) {
	rows, err := s.conn.Query(ctx,
		"SELECT request_id, ts, skeleton, field_names, field_is_array, field_offsets, field_counts, hashes, request_sha256, response_body, request_truncated, response_truncated, project_id, principal_id, relay_key_hash FROM "+
			requestsTable+" WHERE request_id = ? ORDER BY ts DESC LIMIT 1",
		requestID)
	if err != nil {
		return payload.Record{}, fmt.Errorf("payload/clickhouse: get %s query: %w", requestsTable, err)
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
		skeleton, respBody  string
		digest              string
		names, hashes       []string
		isArray             []uint8
		offsets, counts     []uint32
		reqTrunc, respTrunc uint8
	)
	if err := rows.Scan(
		&r.RequestID, &r.Timestamp, &skeleton, &names, &isArray, &offsets, &counts, &hashes,
		&digest, &respBody, &reqTrunc, &respTrunc, &r.ProjectID, &r.PrincipalID, &r.RelayKeyHash,
	); err != nil {
		return payload.Record{}, fmt.Errorf("payload/clickhouse: scan %s row: %w", requestsTable, err)
	}
	if err := rows.Err(); err != nil {
		return payload.Record{}, err
	}
	r.RequestTruncated = reqTrunc == 1
	r.ResponseTruncated = respTrunc == 1
	if respBody != "" {
		r.ResponseBody = []byte(respBody)
	}

	fields, err := fieldsFromColumns(names, isArray, offsets, counts, hashes)
	if err != nil {
		s.log.Error("payload/clickhouse: request row unreadable", "request_id", requestID, "err", err)
		return payload.Record{}, err
	}
	pieces, err := s.fetchPieces(ctx, requestID, r.ProjectID)
	if err != nil {
		return payload.Record{}, err
	}
	body, err := dedup.Rebuild([]byte(skeleton), fields, func(h dedup.Hash) ([]byte, bool) {
		b, ok := pieces[h]
		return b, ok
	})
	if err != nil {
		s.log.Error("payload/clickhouse: rebuild request body", "request_id", requestID, "err", err)
		return payload.Record{}, fmt.Errorf("payload/clickhouse: rebuild request body: %w", err)
	}
	if !bodyMatchesDigest(body, digest) {
		s.log.Error("payload/clickhouse: rebuilt request body does not match its digest", "request_id", requestID)
		return payload.Record{}, fmt.Errorf("%w: request %s", payload.ErrIntegrity, requestID)
	}
	if len(body) > 0 {
		r.RequestBody = body
	}
	return r, nil
}

// fetchPieces loads the pieces of the row getDedup reads, from that row's
// project only. The hash list is read server-side so the query text stays
// small however many pieces a body has.
func (s *Reader) fetchPieces(ctx context.Context, requestID, projectID string) (map[dedup.Hash][]byte, error) {
	rows, err := s.conn.Query(ctx,
		"SELECT hash, any(body) FROM "+piecesTable+" WHERE project_id = ? AND hash IN (SELECT arrayJoin(hashes) FROM (SELECT hashes FROM "+
			requestsTable+" WHERE request_id = ? ORDER BY ts DESC LIMIT 1)) GROUP BY hash",
		projectID, requestID)
	if err != nil {
		return nil, fmt.Errorf("payload/clickhouse: get %s query: %w", piecesTable, err)
	}
	defer rows.Close()
	pieces := map[dedup.Hash][]byte{}
	for rows.Next() {
		var (
			h    dedup.Hash
			body string
		)
		if err := rows.Scan(&h, &body); err != nil {
			return nil, fmt.Errorf("payload/clickhouse: scan %s row: %w", piecesTable, err)
		}
		pieces[h] = []byte(body)
	}
	return pieces, rows.Err()
}
