package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/wyolet/relay/pkg/reqid"
)

type reloader interface {
	Reload(ctx context.Context) error
}

// adminReloadHandler returns an http.HandlerFunc that calls store.Reload.
// token must be non-empty; callers are responsible for not registering when token is empty.
func adminReloadHandler(token string, store reloader) http.HandlerFunc {
	tok := []byte(token)
	return func(w http.ResponseWriter, r *http.Request) {
		id := reqid.From(r.Context())
		ip := r.RemoteAddr

		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(auth), tok) != 1 {
			slog.Info("admin/reload: unauthorized", "request_id", id, "ip", ip)
			http.NotFound(w, r)
			return
		}

		if err := store.Reload(r.Context()); err != nil {
			slog.Info("admin/reload: failed", "request_id", id, "ip", ip, "err", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		slog.Info("admin/reload: ok", "request_id", id, "ip", ip)
		w.WriteHeader(http.StatusOK)
	}
}
