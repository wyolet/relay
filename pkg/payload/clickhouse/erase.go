package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/wyolet/relay/pkg/payload"
)

var _ payload.Eraser = (*Reader)(nil)

var errEmptyEraseFilter = errors.New("payload/clickhouse: erase needs a project or a principal")

// Erase deletes the request rows (both layouts) matching f, then the pieces
// of the affected projects that no remaining request references and that
// were last written before the erase began. Each delete waits until
// ClickHouse has rewritten the affected parts, so the bodies are unreadable
// when Erase returns.
func (s *Reader) Erase(ctx context.Context, f payload.EraseFilter) (payload.EraseResult, error) {
	where, args, err := eraseWhere(f)
	if err != nil {
		return payload.EraseResult{}, err
	}
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"mutations_sync": 2}))

	// Server time, like last_seen: a piece a sink writes from here on (its
	// request row may not exist yet) is newer than this and is kept.
	var startMilli int64
	if err := s.conn.QueryRow(ctx, "SELECT toUnixTimestamp64Milli(now64(3))").Scan(&startMilli); err != nil {
		return payload.EraseResult{}, fmt.Errorf("payload/clickhouse: erase start time: %w", err)
	}

	var res payload.EraseResult
	for _, table := range []string{requestsTable, chTable} {
		var n uint64
		if err := s.conn.QueryRow(ctx, "SELECT count() FROM "+table+" WHERE "+where, args...).Scan(&n); err != nil {
			return payload.EraseResult{}, fmt.Errorf("payload/clickhouse: count %s: %w", table, err)
		}
		res.Requests += n
	}
	projects, err := s.erasedProjects(ctx, f, where, args)
	if err != nil {
		return payload.EraseResult{}, err
	}

	for _, table := range []string{requestsTable, chTable} {
		if err := s.conn.Exec(ctx, "ALTER TABLE "+table+" DELETE WHERE "+where, args...); err != nil {
			return res, fmt.Errorf("payload/clickhouse: erase %s: %w", table, err)
		}
	}
	if err := deleteUnreferencedPieces(ctx, s.conn, projects, startMilli); err != nil {
		if f.ProjectID == "" {
			return res, piecesLeftError(err, f.PrincipalID, projects)
		}
		return res, err
	}
	return res, nil
}

// piecesLeftError tells the operator how to finish a principal-only erase
// whose pieces delete failed: the principal's rows are gone, so the same
// filter would find no project to clean, while a filter naming the project
// always cleans it.
func piecesLeftError(err error, principalID string, projects []string) error {
	quoted := make([]string, len(projects))
	for i, p := range projects {
		quoted[i] = fmt.Sprintf("%q", p)
	}
	return fmt.Errorf("%w; the request rows are erased but their pieces are not: retry with principalId %q and projectId set to each of %s",
		err, principalID, strings.Join(quoted, ", "))
}

// deleteUnreferencedPieces deletes the pieces of projects that no request
// row references and whose newest row is older than startMilli.
func deleteUnreferencedPieces(ctx context.Context, conn clickhouse.Conn, projects []string, startMilli int64) error {
	if len(projects) == 0 {
		return nil
	}
	in := strings.TrimSuffix(strings.Repeat("?, ", len(projects)), ", ")
	ids := make([]any, len(projects))
	for i, p := range projects {
		ids[i] = p
	}
	args := append(append(append([]any{}, ids...), startMilli), ids...)
	err := conn.Exec(ctx,
		"ALTER TABLE "+piecesTable+" DELETE WHERE project_id IN ("+in+") AND last_seen < fromUnixTimestamp64Milli(?)"+
			" AND (project_id, hash) NOT IN (SELECT project_id, arrayJoin(hashes) FROM "+requestsTable+" WHERE project_id IN ("+in+"))",
		args...)
	if err != nil {
		return fmt.Errorf("payload/clickhouse: erase unreferenced %s: %w", piecesTable, err)
	}
	return nil
}

// latestPiecesDelete returns the number of the newest delete mutation on
// payload_pieces, or 0 when there is none. For a MergeTree table mutation_id
// is "mutation_<block number>.txt" (a zero-padded counter when replicated);
// both only grow, so the newest one changes on every delete even after
// ClickHouse trims finished mutations from system.mutations.
func latestPiecesDelete(ctx context.Context, conn clickhouse.Conn) (uint64, error) {
	var n uint64
	err := conn.QueryRow(ctx,
		"SELECT max(toUInt64OrZero(extract(mutation_id, '[0-9]+'))) FROM system.mutations"+
			" WHERE database = currentDatabase() AND table = ? AND startsWith(command, 'DELETE')",
		piecesTable).Scan(&n)
	return n, err
}

// erasedProjects returns the projects an erase can orphan pieces in: the
// filter's project, or every project the principal's request rows are in.
// The filter's project is returned even when no row matches, so a retry
// after a failed pieces delete still cleans it.
func (s *Reader) erasedProjects(ctx context.Context, f payload.EraseFilter, where string, args []any) ([]string, error) {
	if f.ProjectID != "" {
		return []string{f.ProjectID}, nil
	}
	rows, err := s.conn.Query(ctx, "SELECT DISTINCT project_id FROM "+requestsTable+" WHERE "+where, args...)
	if err != nil {
		return nil, fmt.Errorf("payload/clickhouse: projects to erase: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("payload/clickhouse: scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// eraseWhere renders f over the owner columns every request table carries.
// An empty filter is refused: it would match every row.
func eraseWhere(f payload.EraseFilter) (string, []any, error) {
	var conds []string
	var args []any
	if f.ProjectID != "" {
		conds, args = append(conds, "project_id = ?"), append(args, f.ProjectID)
	}
	if f.PrincipalID != "" {
		conds, args = append(conds, "principal_id = ?"), append(args, f.PrincipalID)
	}
	if len(conds) == 0 {
		return "", nil, errEmptyEraseFilter
	}
	return strings.Join(conds, " AND "), args, nil
}
