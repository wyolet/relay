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

	"github.com/wyolet/relay/internal/db"
	"github.com/wyolet/relay/pkg/admin/crud"
	"github.com/wyolet/relay/pkg/configstore"
)

// ensure chi is used (URL params in handlers)
var _ = chi.URLParam

// --- Secret response type (never carries cleartext) ---

type secretValueFromResponse struct {
	Kind       string `json:"kind"`
	Env        string `json:"env,omitempty"`
	ValueMasked string `json:"value_masked,omitempty"`
}

type secretResponse struct {
	Name      string                  `json:"name"`
	ValueFrom secretValueFromResponse `json:"valueFrom"`
}

// maskValue returns a masked representation of a cleartext key value.
// Recognized provider prefixes (sk-, gsk_, etc.) are preserved; remainder replaced.
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

// secretToResponse converts a configstore.Secret (post-reload snapshot) to secretResponse.
// The snapshot's Resolved field carries the decrypted cleartext for stored-mode; we mask it on-demand.
func secretToResponse(sec *configstore.Secret) secretResponse {
	if sec.Spec.ValueFrom != nil && sec.Spec.ValueFrom.Env != "" {
		return secretResponse{
			Name: sec.Metadata.Name,
			ValueFrom: secretValueFromResponse{
				Kind: "env",
				Env:  sec.Spec.ValueFrom.Env,
			},
		}
	}
	// stored-mode: mask Resolved
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

// --- Secret handler factories (manual, not using Kind[T] since shapes diverge) ---

func secretListHandler(store *configstore.PGStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secrets := store.Secrets()
		out := make([]secretResponse, 0, len(secrets))
		for _, s := range secrets {
			out = append(out, secretToResponse(s))
		}
		adminWriteJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

func secretGetHandler(store *configstore.PGStore) http.HandlerFunc {
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

func applySecretWrite(ctx context.Context, store *configstore.PGStore, tx pgx.Tx, name string, inp secretInput) error {
	meta := configstore.Metadata{Name: name}
	switch inp.ValueFrom.Kind {
	case "env":
		return store.UpsertSecretEnv(ctx, tx, name, inp.ValueFrom.Env, inp.Provider, meta)
	case "stored":
		return store.UpsertSecretStored(ctx, tx, name, inp.ValueFrom.Value, inp.Provider, meta)
	default:
		return fmt.Errorf("unknown kind %q", inp.ValueFrom.Kind)
	}
}

func secretCreateHandler(store *configstore.PGStore, deps crud.Deps) http.HandlerFunc {
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

func secretUpdateHandler(store *configstore.PGStore, deps crud.Deps) http.HandlerFunc {
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

func secretDeleteHandler(store *configstore.PGStore, deps crud.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		name := chi.URLParam(r, "name")

		if _, ok := store.SecretByName(name); !ok {
			adminWriteErr(w, http.StatusNotFound, "invalid_request_error", "not_found",
				fmt.Sprintf("Secret %q not found", name))
			return
		}

		if verr := deps.Patcher.ValidateWithPatch(configstore.Patch{DeleteSecret: name}); verr != nil {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "validation_failed", verr.Error())
			return
		}

		tx, err := deps.Pool.Begin(ctx)
		if err != nil {
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", "begin tx: "+err.Error())
			return
		}

		if err := db.New(tx).DeleteSecret(ctx, name); err != nil {
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

// --- Attachment handlers ---

// attachmentResponse is the shape for a single attachment record.
// id is the composite key encoded as "parentKind:parentName:ratelimitName:meter".
type attachmentResponse struct {
	ID            string `json:"id"`
	ParentKind    string `json:"parentKind"`
	ParentName    string `json:"parentName"`
	RatelimitName string `json:"ratelimitName"`
	Meter         string `json:"meter"`
}

// attachmentInput is the POST /admin/attachments request body.
type attachmentInput struct {
	ParentKind    string `json:"parentKind"`
	ParentName    string `json:"parentName"`
	RatelimitName string `json:"ratelimitName"`
	Meter         string `json:"meter"`
}

func attachmentID(parentKind, parentName, rlName, meter string) string {
	return parentKind + ":" + parentName + ":" + rlName + ":" + meter
}

func parseAttachmentID(id string) (parentKind, parentName, rlName, meter string, err error) {
	parts := strings.SplitN(id, ":", 4)
	if len(parts) != 4 {
		return "", "", "", "", fmt.Errorf("invalid attachment id %q: expected parentKind:parentName:ratelimitName:meter", id)
	}
	return parts[0], parts[1], parts[2], parts[3], nil
}

func dbAttachmentToResponse(a db.Attachment) attachmentResponse {
	return attachmentResponse{
		ID:            attachmentID(a.ParentKind, a.ParentName, a.RatelimitName, a.Meter),
		ParentKind:    a.ParentKind,
		ParentName:    a.ParentName,
		RatelimitName: a.RatelimitName,
		Meter:         a.Meter,
	}
}


func attachmentListHandler(store *configstore.PGStore, _ crud.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		parentKind := r.URL.Query().Get("parent_kind")
		parentName := r.URL.Query().Get("parent_name")

		if parentKind == "" || parentName == "" {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "invalid_query",
				"parent_kind and parent_name query params required")
			return
		}

		rows, err := store.ListAttachmentsByParent(ctx, parentKind, parentName)
		if err != nil {
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
			return
		}
		out := make([]attachmentResponse, 0, len(rows))
		for _, a := range rows {
			out = append(out, dbAttachmentToResponse(a))
		}
		adminWriteJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

func attachmentCreateHandler(store *configstore.PGStore, deps crud.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		raw, err := io.ReadAll(r.Body)
		if err != nil {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", err.Error())
			return
		}
		var inp attachmentInput
		if err := json.Unmarshal(raw, &inp); err != nil {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", err.Error())
			return
		}
		if inp.ParentKind == "" || inp.ParentName == "" || inp.RatelimitName == "" || inp.Meter == "" {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "invalid_body",
				"parentKind, parentName, ratelimitName, meter all required")
			return
		}

		// Validate that the ratelimit exists in the snapshot.
		if _, ok := store.RateLimitByName(inp.RatelimitName); !ok {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "validation_failed",
				fmt.Sprintf("ratelimit %q not found", inp.RatelimitName))
			return
		}

		// Validate that the parent exists.
		switch configstore.Kind(inp.ParentKind) {
		case configstore.KindPool:
			if _, ok := store.PoolByName(inp.ParentName); !ok {
				adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "validation_failed",
					fmt.Sprintf("Pool %q not found", inp.ParentName))
				return
			}
		case configstore.KindSecret:
			if _, ok := store.SecretByName(inp.ParentName); !ok {
				adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "validation_failed",
					fmt.Sprintf("Secret %q not found", inp.ParentName))
				return
			}
		case configstore.KindModel:
			if _, ok := store.ModelByName(inp.ParentName); !ok {
				adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "validation_failed",
					fmt.Sprintf("Model %q not found", inp.ParentName))
				return
			}
		default:
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "validation_failed",
				fmt.Sprintf("parentKind %q not supported (must be Pool, Secret, or Model)", inp.ParentKind))
			return
		}

		tx, err := deps.Pool.Begin(ctx)
		if err != nil {
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", "begin tx: "+err.Error())
			return
		}

		q := db.New(tx)
		row, err := q.InsertAttachment(ctx, db.InsertAttachmentParams{
			ParentKind:    inp.ParentKind,
			ParentName:    inp.ParentName,
			RatelimitName: inp.RatelimitName,
			Meter:         inp.Meter,
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
			return
		}

		// Also update the parent's spec to embed the attachment.
		if err := addAttachmentToParentSpec(ctx, tx, inp); err != nil {
			_ = tx.Rollback(ctx)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error",
				"update parent spec: "+err.Error())
			return
		}

		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(ctx)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", "commit: "+err.Error())
			return
		}

		if err := deps.Reloader.Reload(ctx); err != nil {
			deps.Logger.ErrorContext(ctx, "admin: reload failed after attachment create", "err", err)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "reload_failed",
				"mutation committed but reload failed: "+err.Error())
			return
		}

		id := attachmentID(inp.ParentKind, inp.ParentName, inp.RatelimitName, inp.Meter)
		adminEmitAudit(deps.Logger, ctx, r, "Attachment", id, "create")
		adminWriteJSON(w, http.StatusCreated, dbAttachmentToResponse(row))
	}
}

func attachmentDeleteHandler(store *configstore.PGStore, deps crud.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		rawID := chi.URLParam(r, "id")

		parentKind, parentName, rlName, meter, err := parseAttachmentID(rawID)
		if err != nil {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "invalid_id", err.Error())
			return
		}

		tx, err := deps.Pool.Begin(ctx)
		if err != nil {
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", "begin tx: "+err.Error())
			return
		}

		q := db.New(tx)
		n, err := q.DeleteAttachmentByCompositeKey(ctx, db.DeleteAttachmentByCompositeKeyParams{
			ParentKind:    parentKind,
			ParentName:    parentName,
			RatelimitName: rlName,
			Meter:         meter,
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
			return
		}
		if n == 0 {
			_ = tx.Rollback(ctx)
			adminWriteErr(w, http.StatusNotFound, "invalid_request_error", "not_found",
				fmt.Sprintf("attachment %q not found", rawID))
			return
		}

		// Remove from parent's spec.
		if err := removeAttachmentFromParentSpec(ctx, tx, parentKind, parentName, rlName, meter); err != nil {
			_ = tx.Rollback(ctx)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error",
				"update parent spec: "+err.Error())
			return
		}

		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(ctx)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "internal_error", "commit: "+err.Error())
			return
		}

		if err := deps.Reloader.Reload(ctx); err != nil {
			deps.Logger.ErrorContext(ctx, "admin: reload failed after attachment delete", "err", err)
			adminWriteErr(w, http.StatusInternalServerError, "server_error", "reload_failed",
				"mutation committed but reload failed: "+err.Error())
			return
		}

		adminEmitAudit(deps.Logger, ctx, r, "Attachment", rawID, "delete")
		w.WriteHeader(http.StatusNoContent)
	}
}

// addAttachmentToParentSpec appends a RateLimitAttachment to the parent resource's spec JSON.
func addAttachmentToParentSpec(ctx context.Context, tx pgx.Tx, inp attachmentInput) error {
	entry := configstore.RateLimitAttachment{
		Ref:   inp.RatelimitName,
		Meter: configstore.Meter(inp.Meter),
	}
	switch configstore.Kind(inp.ParentKind) {
	case configstore.KindPool:
		return modifyPoolRateLimits(ctx, tx, inp.ParentName, func(current []configstore.RateLimitAttachment) []configstore.RateLimitAttachment {
			for _, a := range current {
				if a.Ref == entry.Ref && a.Meter == entry.Meter {
					return current // already present
				}
			}
			return append(current, entry)
		})
	case configstore.KindSecret:
		return modifySecretRateLimits(ctx, tx, inp.ParentName, func(current []configstore.RateLimitAttachment) []configstore.RateLimitAttachment {
			for _, a := range current {
				if a.Ref == entry.Ref && a.Meter == entry.Meter {
					return current
				}
			}
			return append(current, entry)
		})
	case configstore.KindModel:
		return modifyModelRateLimits(ctx, tx, inp.ParentName, func(current []configstore.RateLimitAttachment) []configstore.RateLimitAttachment {
			for _, a := range current {
				if a.Ref == entry.Ref && a.Meter == entry.Meter {
					return current
				}
			}
			return append(current, entry)
		})
	}
	return nil
}

// removeAttachmentFromParentSpec removes a RateLimitAttachment from the parent's spec JSON.
func removeAttachmentFromParentSpec(ctx context.Context, tx pgx.Tx, parentKind, parentName, rlName, meter string) error {
	filter := func(current []configstore.RateLimitAttachment) []configstore.RateLimitAttachment {
		out := current[:0]
		for _, a := range current {
			if a.Ref == rlName && string(a.Meter) == meter {
				continue
			}
			out = append(out, a)
		}
		return out
	}
	switch configstore.Kind(parentKind) {
	case configstore.KindPool:
		return modifyPoolRateLimits(ctx, tx, parentName, filter)
	case configstore.KindSecret:
		return modifySecretRateLimits(ctx, tx, parentName, filter)
	case configstore.KindModel:
		return modifyModelRateLimits(ctx, tx, parentName, filter)
	}
	return nil
}

func modifyPoolRateLimits(ctx context.Context, tx pgx.Tx, name string, fn func([]configstore.RateLimitAttachment) []configstore.RateLimitAttachment) error {
	var specRaw []byte
	row := tx.QueryRow(ctx, `SELECT spec FROM pools WHERE name=$1`, name)
	if err := row.Scan(&specRaw); err != nil {
		return err
	}
	var spec configstore.PoolSpec
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		return err
	}
	spec.RateLimits = fn(spec.RateLimits)
	updated, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE pools SET spec=$1, updated_at=NOW() WHERE name=$2`, updated, name)
	return err
}

func modifySecretRateLimits(ctx context.Context, tx pgx.Tx, name string, fn func([]configstore.RateLimitAttachment) []configstore.RateLimitAttachment) error {
	var specRaw []byte
	row := tx.QueryRow(ctx, `SELECT spec FROM secrets WHERE name=$1`, name)
	if err := row.Scan(&specRaw); err != nil {
		return err
	}
	var spec configstore.SecretSpec
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		return err
	}
	spec.RateLimits = fn(spec.RateLimits)
	updated, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE secrets SET spec=$1, updated_at=NOW() WHERE name=$2`, updated, name)
	return err
}

func modifyModelRateLimits(ctx context.Context, tx pgx.Tx, name string, fn func([]configstore.RateLimitAttachment) []configstore.RateLimitAttachment) error {
	var specRaw []byte
	row := tx.QueryRow(ctx, `SELECT spec FROM models WHERE name=$1`, name)
	if err := row.Scan(&specRaw); err != nil {
		return err
	}
	var spec configstore.ModelSpec
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		return err
	}
	spec.RateLimits = fn(spec.RateLimits)
	updated, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE models SET spec=$1, updated_at=NOW() WHERE name=$2`, updated, name)
	return err
}
