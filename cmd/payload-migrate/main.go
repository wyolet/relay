// Command payload-migrate backfills a JSONL file-backend payload dump
// (relay-payloads.jsonl) into the ClickHouse payload_logs table, so historical
// request/response bodies become queryable through the same reader the relay
// uses — after which the multi-GB local file can be deleted.
//
// It reuses payload.Record, so the file's base64-encoded bodies decode to raw
// bytes and the RFC3339 timestamp parses natively; rows are appended in the
// exact column order the live ClickHouse sink and reader use, guaranteeing the
// migrated rows are readable via the relay's payload Get/List.
//
// One-shot and resumable: every request_id already present in payload_logs is
// loaded up front and skipped, so re-running after an interruption only inserts
// the remainder (and never double-inserts). Bodies are MB-scale, so inserts
// flush on a byte budget as well as a row count.
//
//	go run ./cmd/payload-migrate -file relay-payloads.jsonl   # DSN from $RELAY_CH_DSN
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"os"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/wyolet/relay/pkg/payload"
)

const chTable = "payload_logs"

func main() {
	var (
		file      = flag.String("file", "relay-payloads.jsonl", "JSONL payload dump to migrate")
		dsn       = flag.String("dsn", os.Getenv("RELAY_CH_DSN"), "ClickHouse DSN (default $RELAY_CH_DSN)")
		batchRows = flag.Int("batch", 50, "max rows per insert batch")
		batchMB   = flag.Int("batch-mb", 32, "max accumulated body MB per insert batch")
	)
	flag.Parse()

	if *dsn == "" {
		log.Fatal("payload-migrate: no DSN (pass -dsn or set RELAY_CH_DSN)")
	}
	f, err := os.Open(*file)
	if err != nil {
		log.Fatalf("payload-migrate: open %s: %v", *file, err)
	}
	defer f.Close()

	ctx := context.Background()
	conn := mustConnect(ctx, *dsn)
	defer conn.Close()

	seen := loadExistingIDs(ctx, conn)
	log.Printf("payload-migrate: %d request_ids already in %s (will skip)", len(seen), chTable)

	var read, inserted, skipped, bad int
	var batch driver.Batch
	var batchN, batchBytes int
	flushBudget := *batchMB << 20

	newBatch := func() {
		// Use the long-lived ctx: PrepareBatch binds the context to the batch
		// and Send() reuses it, so a per-batch timeout cancel() would abort the
		// later Send mid-migration.
		var berr error
		if batch, berr = conn.PrepareBatch(ctx, "INSERT INTO "+chTable); berr != nil {
			log.Fatalf("payload-migrate: prepare batch: %v", berr)
		}
		batchN, batchBytes = 0, 0
	}
	flush := func() {
		if batchN == 0 {
			return
		}
		if serr := batch.Send(); serr != nil {
			log.Fatalf("payload-migrate: send batch (after %d inserted): %v", inserted, serr)
		}
		inserted += batchN
		log.Printf("payload-migrate: progress read=%d inserted=%d skipped=%d bad=%d", read, inserted, skipped, bad)
		newBatch()
	}

	newBatch()
	// ReadBytes (not bufio.Scanner) handles the MB-scale lines without a
	// token-size ceiling. Read to EOF, processing the trailing partial line.
	rd := bufio.NewReaderSize(f, 1<<20)
	for {
		line, rerr := rd.ReadBytes('\n')
		if len(line) > 1 { // skip blank/"\n" lines
			read++
			var r payload.Record
			if json.Unmarshal(line, &r) != nil || r.RequestID == "" {
				bad++
			} else if _, dup := seen[r.RequestID]; dup {
				skipped++
			} else {
				seen[r.RequestID] = struct{}{} // also dedups within the file
				if aerr := batch.Append(
					r.RequestID,
					r.Timestamp,
					string(r.RequestBody),
					string(r.ResponseBody),
					b2u8(r.RequestTruncated),
					b2u8(r.ResponseTruncated),
				); aerr != nil {
					log.Fatalf("payload-migrate: append %s: %v", r.RequestID, aerr)
				}
				batchN++
				batchBytes += len(r.RequestBody) + len(r.ResponseBody)
				if batchN >= *batchRows || batchBytes >= flushBudget {
					flush()
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			log.Fatalf("payload-migrate: read %s: %v", *file, rerr)
		}
	}
	flush()

	total := count(ctx, conn)
	log.Printf("payload-migrate: DONE read=%d inserted=%d skipped=%d bad=%d | %s now has %d rows",
		read, inserted, skipped, bad, chTable, total)
}

func mustConnect(ctx context.Context, dsn string) clickhouse.Conn {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		log.Fatalf("payload-migrate: parse DSN: %v", err)
	}
	opts.MaxOpenConns = 4
	conn, err := clickhouse.Open(opts)
	if err != nil {
		log.Fatalf("payload-migrate: open: %v", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.Ping(pctx); err != nil {
		log.Fatalf("payload-migrate: ping (is the CH DSN reachable?): %v", err)
	}
	return conn
}

// loadExistingIDs makes the migration resumable/idempotent: any request_id
// already stored is skipped on this run.
func loadExistingIDs(ctx context.Context, conn clickhouse.Conn) map[string]struct{} {
	rows, err := conn.Query(ctx, "SELECT DISTINCT request_id FROM "+chTable)
	if err != nil {
		log.Fatalf("payload-migrate: load existing ids (does %s exist? start the relay with backend=clickhouse first): %v", chTable, err)
	}
	defer rows.Close()
	set := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			log.Fatalf("payload-migrate: scan existing id: %v", err)
		}
		set[id] = struct{}{}
	}
	return set
}

func count(ctx context.Context, conn clickhouse.Conn) uint64 {
	var n uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM "+chTable).Scan(&n); err != nil {
		log.Printf("payload-migrate: count failed: %v", err)
	}
	return n
}

func b2u8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}
