package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/admin/crud"
	"github.com/wyolet/relay/internal/catalog"
)

// ensure chi is used (URL params in handlers)
var _ = chi.URLParam

// --- Secret response type (never carries cleartext) ---

type secretValueFromResponse struct {
	Kind        string `json:"kind"`
	Env         string `json:"env,omitempty"`
	ValueMasked string `json:"value_masked,omitempty"`
}

type secretResponse struct {
	Name      string                  `json:"name"`
	ValueFrom secretValueFromResponse `json:"valueFrom"`
}

// maskValue returns a masked representation of a cleartext key value.
func maskValue(cleartext string) string {
	if len(cleartext) == 0 {
		return "***"
	}
	last4 := cleartext
	if len(cleartext) > 4 {
		last4 = cleartext[len(cleartext)-4:]
	}
	prefixes := []string{"sk-", "gsk_", "xai-", "ant-", "hf_"}
	for _, p := range prefixes {
		if strings.HasPrefix(cleartext, p) {
			return p + "..." + last4
		}
	}
	return "***..." + last4
}

// secretToResponse converts a catalog.Secret to secretResponse (no cleartext).
func secretToResponse(sec *catalog.Secret) secretResponse {
	if sec.Spec.ValueFrom != nil && sec.Spec.ValueFrom.Env != "" {
		return secretResponse{
			Name: sec.Metadata.Name,
			ValueFrom: secretValueFromResponse{
				Kind: "env",
				Env:  sec.Spec.ValueFrom.Env,
			},
		}
	}
	return secretResponse{
		Name: sec.Metadata.Name,
		ValueFrom: secretValueFromResponse{
			Kind:        "stored",
			ValueMasked: maskValue(sec.Resolved),
		},
	}
}

// --- Secret request types ---

type secretValueFromInput struct {
	Kind  string `json:"kind"`
	Env   string `json:"env,omitempty"`
	Value string `json:"value,omitempty"`
}

type secretInput struct {
	Name      string               `json:"name"`
	Provider  string               `json:"provider"`
	ValueFrom secretValueFromInput `json:"valueFrom"`
}

// --- Secret handler factories ---

func secretListHandler(store *catalog.PGStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secrets := store.Secrets()
		out := make([]secretResponse, 0, len(secrets))
		for _, s := range secrets {
			out = append(out, secretToResponse(s))
		}
		adminWriteJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

func secretGetHandler(store *catalog.PGStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		sec, ok := store.SecretByName(name)
		if !ok {
			adminWriteErr(w, http.StatusNotFound, "invalid_request_error", "not_found",
				fmt.Sprintf("Secret %q not found", name))
			return
		}
		adminWriteJSON(w, http.StatusOK, secretToResponse(sec))
	}
}

func decodeSecretInput(r *http.Request) (secretInput, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return secretInput{}, err
	}
	var v secretInput
	if err := json.Unmarshal(raw, &v); err != nil {
		return secretInput{}, err
	}
	if v.Name == "" {
		return secretInput{}, errors.New("name required")
	}
	if v.ValueFrom.Kind != "env" && v.ValueFrom.Kind != "stored" {
		return secretInput{}, fmt.Errorf("valueFrom.kind must be \"env\" or \"stored\", got %q", v.ValueFrom.Kind)
	}
	if v.ValueFrom.Kind == "env" && v.ValueFrom.Env == "" {
		return secretInput{}, errors.New("valueFrom.env required for env-mode")
	}
	if v.ValueFrom.Kind == "stored" && v.ValueFrom.Value == "" {
		return secretInput{}, errors.New("valueFrom.value required for stored-mode")
	}
	if v.Provider == "" {
		v.Provider = "default"
	}
	return v, nil
}

func applySecretWrite(ctx context.Context, store *catalog.PGStore, tx pgx.Tx, name string, inp secretInput) error {
	meta := catalog.Metadata{Name: name}
	switch inp.ValueFrom.Kind {
	case "env":
		return store.UpsertSecretEnv(ctx, tx, name, inp.ValueFrom.Env, inp.Provider, meta)
	case "stored":
		return store.UpsertSecretStored(ctx, tx, name, inp.ValueFrom.Value, inp.Provider, meta)
	default:
		return fmt.Errorf("unknown kind %q", inp.ValueFrom.Kind)
	}
}

func secretCreateHandler(store *catalog.PGStore, deps crud.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		inp, err := decodeSecretInput(r)
		if err != nil {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", err.Error())
			return
		}

		if inp.ValueFrom.Kind == "stored" && !store.HasMasterKey() {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "master_key_required",
				"stored-mode secret requires RELAY_MASTER_KEY to be set")
			return
		}

		tx, err := deps.Pool.Begin(ctx)
		if err != nil {
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", "begin tx: "+err.Error())
			return
		}

		if err := applySecretWrite(ctx, store, tx, inp.Name, inp); err != nil {
			_ = tx.Rollback(ctx)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
			return
		}

		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(ctx)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", "commit: "+err.Error())
			return
		}

		if err := deps.Reloader.Reload(ctx); err != nil {
			deps.Logger.ErrorContext(ctx, "admin: reload failed after secret create", "name", inp.Name, "err", err)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "reload_failed",
				"mutation committed but reload failed: "+err.Error())
			return
		}

		adminEmitAudit(deps.Logger, ctx, r, "Secret", inp.Name, "create")

		sec, ok := store.SecretByName(inp.Name)
		if !ok {
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error",
				"created but could not read back")
			return
		}
		adminWriteJSON(w, http.StatusCreated, secretToResponse(sec))
	}
}

func secretUpdateHandler(store *catalog.PGStore, deps crud.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		name := chi.URLParam(r, "name")

		if _, ok := store.SecretByName(name); !ok {
			adminWriteErr(w, http.StatusNotFound, "invalid_request_error", "not_found",
				fmt.Sprintf("Secret %q not found", name))
			return
		}

		inp, err := decodeSecretInput(r)
		if err != nil {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", err.Error())
			return
		}
		inp.Name = name // URL param wins

		if inp.ValueFrom.Kind == "stored" && !store.HasMasterKey() {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "master_key_required",
				"stored-mode secret requires RELAY_MASTER_KEY to be set")
			return
		}

		tx, err := deps.Pool.Begin(ctx)
		if err != nil {
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", "begin tx: "+err.Error())
			return
		}

		if err := applySecretWrite(ctx, store, tx, name, inp); err != nil {
			_ = tx.Rollback(ctx)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
			return
		}

		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(ctx)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", "commit: "+err.Error())
			return
		}

		if err := deps.Reloader.Reload(ctx); err != nil {
			deps.Logger.ErrorContext(ctx, "admin: reload failed after secret update", "name", name, "err", err)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "reload_failed",
				"mutation committed but reload failed: "+err.Error())
			return
		}

		adminEmitAudit(deps.Logger, ctx, r, "Secret", name, "update")

		sec, ok := store.SecretByName(name)
		if !ok {
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error",
				"updated but could not read back")
			return
		}
		adminWriteJSON(w, http.StatusOK, secretToResponse(sec))
	}
}

func secretDeleteHandler(store *catalog.PGStore, deps crud.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		name := chi.URLParam(r, "name")

		if _, ok := store.SecretByName(name); !ok {
			adminWriteErr(w, http.StatusNotFound, "invalid_request_error", "not_found",
				fmt.Sprintf("Secret %q not found", name))
			return
		}

		if verr := deps.Patcher.ValidateWithPatch(catalog.Patch{DeleteSecret: name}); verr != nil {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "validation_failed", verr.Error())
			return
		}

		tx, err := deps.Pool.Begin(ctx)
		if err != nil {
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", "begin tx: "+err.Error())
			return
		}

		if err := gen.New(tx).DeleteSecret(ctx, name); err != nil {
			_ = tx.Rollback(ctx)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
			return
		}

		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(ctx)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", "commit: "+err.Error())
			return
		}

		if err := deps.Reloader.Reload(ctx); err != nil {
			deps.Logger.ErrorContext(ctx, "admin: reload failed after secret delete", "name", name, "err", err)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "reload_failed",
				"mutation committed but reload failed: "+err.Error())
			return
		}

		adminEmitAudit(deps.Logger, ctx, r, "Secret", name, "delete")
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- Attachment response type ---

// attachmentResponse is the shape for a single attachment record.
// id is the composite key encoded as "parentKind:parentName:ratelimitName:meter".
type attachmentResponse struct {
	ID            string `json:"id"`
	ParentKind    string `json:"parentKind"`
	ParentName    string `json:"parentName"`
	RatelimitName string `json:"ratelimitName"`
	Meter         string `json:"meter"`
}

func attachmentID(parentKind, parentName, rlName, meter string) string {
	return parentKind + ":" + parentName + ":" + rlName + ":" + meter
}

// attachmentListHandler is a read-only audit endpoint that derives attachment rows
// from the in-memory snapshot (Pools, Secrets, Models inline rateLimits).
// Optional query params parent_kind + parent_name (both required together) filter to one parent.
// The attachments DB table no longer exists; this view is derived entirely from inline spec data.
func attachmentListHandler(store *catalog.PGStore, _ crud.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		parentKind := r.URL.Query().Get("parent_kind")
		parentName := r.URL.Query().Get("parent_name")

		// Both or neither must be supplied.
		if (parentKind == "") != (parentName == "") {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "invalid_query",
				"parent_kind and parent_name must be provided together (or both omitted to list all)")
			return
		}

		// Unknown parentKind with a non-empty filter.
		if parentKind != "" {
			wantKind := catalog.Kind(parentKind)
			if wantKind != catalog.KindPool && wantKind != catalog.KindSecret && wantKind != catalog.KindModel {
				adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "invalid_query",
					fmt.Sprintf("parent_kind %q not supported (must be Pool, Secret, or Model)", parentKind))
				return
			}
		}

		var out []attachmentResponse

		emit := func(kind, name string, rls []catalog.RateLimitAttachment) {
			for _, a := range rls {
				out = append(out, attachmentResponse{
					ID:            attachmentID(kind, name, a.Ref, string(a.Meter)),
					ParentKind:    kind,
					ParentName:    name,
					RatelimitName: a.Ref,
					Meter:         string(a.Meter),
				})
			}
		}

		wantKind := catalog.Kind(parentKind)

		if parentKind == "" || wantKind == catalog.KindPool {
			for _, p := range store.Pools() {
				if parentName != "" && p.Metadata.Name != parentName {
					continue
				}
				emit(string(catalog.KindPool), p.Metadata.Name, p.Spec.RateLimits)
			}
		}
		if parentKind == "" || wantKind == catalog.KindSecret {
			for _, s := range store.Secrets() {
				if parentName != "" && s.Metadata.Name != parentName {
					continue
				}
				emit(string(catalog.KindSecret), s.Metadata.Name, s.Spec.RateLimits)
			}
		}
		if parentKind == "" || wantKind == catalog.KindModel {
			for _, m := range store.Models() {
				if parentName != "" && m.Metadata.Name != parentName {
					continue
				}
				emit(string(catalog.KindModel), m.Metadata.Name, m.Spec.RateLimits)
			}
		}

		if out == nil {
			out = []attachmentResponse{}
		}
		adminWriteJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}
