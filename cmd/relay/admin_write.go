package main

// admin_write.go provides HTTP response helpers for admin handlers that live
// outside the crud factory (admin_login.go) so they don't need to reach
// into the unexported crud-package helpers.

import (
	"encoding/json"
	"net/http"
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
