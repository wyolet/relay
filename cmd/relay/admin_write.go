package main

// admin_write.go provides HTTP response helpers for admin handlers that live
// outside the crud factory (Secret + Attachment) so they don't need to reach
// into the unexported crud-package helpers.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/wyolet/relay/pkg/reqid"
)

func adminWriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func adminWriteErr(w http.ResponseWriter, status int, errType, code, message string) {
	adminWriteJSON(w, status, map[string]any{
		"error": map[string]string{
			"type":    errType,
			"code":    code,
			"message": message,
		},
	})
}

func adminEmitAudit(log *slog.Logger, ctx context.Context, r *http.Request, kind, name, action string) {
	tok := r.Header.Get("X-Relay-Admin-Token")
	if tok == "" {
		tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	sum := sha256.Sum256([]byte(tok))
	hash := fmt.Sprintf("%x", sum[:6])

	log.InfoContext(ctx, "admin: "+strings.ToLower(kind)+" "+action,
		"kind", kind,
		"name", name,
		"action", action,
		"token_hash", hash,
		"request_id", reqid.From(ctx),
	)
}
