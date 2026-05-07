package storage

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// CatalogWatcher subscribes to PG NOTIFY relay_catalog and invokes onNotify
// for every notification. Construct via NewCatalogWatcher; close via Close.
//
// The watcher holds a dedicated *pgx.Conn (NOT from the pool — LISTEN occupies
// a connection for its lifetime). Reconnects on connection loss with backoff.
type CatalogWatcher struct {
	dsn      string
	onNotify func()
	cancel   context.CancelFunc
	done     chan struct{}
	log      *slog.Logger
}

// NewCatalogWatcher opens a dedicated PG connection, issues LISTEN relay_catalog,
// and starts the background goroutine. The caller must call Close to stop it.
func NewCatalogWatcher(ctx context.Context, dsn string, onNotify func(), log *slog.Logger) (*CatalogWatcher, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "LISTEN relay_catalog"); err != nil {
		_ = conn.Close(ctx)
		return nil, err
	}

	watchCtx, cancel := context.WithCancel(context.Background())
	w := &CatalogWatcher{
		dsn:      dsn,
		onNotify: onNotify,
		cancel:   cancel,
		done:     make(chan struct{}),
		log:      log,
	}

	go w.run(watchCtx, conn)
	return w, nil
}

// Close cancels the watcher goroutine and waits for it to exit.
func (w *CatalogWatcher) Close() error {
	w.cancel()
	<-w.done
	return nil
}

// run is the background goroutine. It holds the dedicated conn and reconnects
// on failure with exponential backoff (1s → 2s → 4s, capped at 30s).
func (w *CatalogWatcher) run(ctx context.Context, conn *pgx.Conn) {
	defer close(w.done)
	defer func() {
		if conn != nil {
			_ = conn.Close(context.Background())
		}
	}()

	for {
		notif, err := conn.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// Context cancelled — clean shutdown.
				return
			}
			w.log.Warn("cluster: NOTIFY watcher: connection lost, reconnecting", "err", err)
			_ = conn.Close(context.Background())
			conn = nil

			conn = w.reconnect(ctx)
			if conn == nil {
				// Context was cancelled during reconnect.
				return
			}
			continue
		}
		w.log.Debug("cluster: received relay_catalog notification", "channel", notif.Channel)
		w.onNotify()
	}
}

// reconnect attempts to re-establish the dedicated LISTEN connection using
// exponential backoff (1s → 2s → 4s … 30s cap). Returns nil if ctx is done.
func (w *CatalogWatcher) reconnect(ctx context.Context) *pgx.Conn {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}

		conn, err := pgx.Connect(ctx, w.dsn)
		if err != nil {
			w.log.Warn("cluster: NOTIFY watcher: reconnect failed", "err", err, "next_backoff", backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		if _, err := conn.Exec(ctx, "LISTEN relay_catalog"); err != nil {
			w.log.Warn("cluster: NOTIFY watcher: LISTEN failed after reconnect", "err", err)
			_ = conn.Close(ctx)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		w.log.Info("cluster: NOTIFY watcher: reconnected successfully")
		return conn
	}
}
