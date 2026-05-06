package main

import (
	"net/http"

	"github.com/wyolet/relay/pkg/crypto"
)

// versionHandler returns the relay version + UI version pinned for this build.
func versionHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminWriteJSON(w, http.StatusOK, map[string]any{
			"version": relayVersion,
		})
	}
}

// masterKeyGenerateHandler returns a freshly generated 32-byte master key,
// base64-encoded. This is the ONE place the API ever returns a master key —
// the operator must store it in their orchestrator's secret store; relay
// never persists it. After this response, it cannot be recovered.
func masterKeyGenerateHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, err := crypto.GenerateMasterKey()
		if err != nil {
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
			return
		}
		adminWriteJSON(w, http.StatusOK, map[string]any{"key": key})
	}
}
