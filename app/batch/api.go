package batch

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/httpapi/inference"
)

// Routes returns the /v1/batches HTTP surface. Mount it on the inference router
// behind the readiness → classify → relay-key-auth chain so every handler sees
// an authenticated relay key via inference.RelayKeyFromContext.
//
//	POST   /v1/batches            submit (JSON: {shape, requests:[...]})
//	GET    /v1/batches/{id}       status + per-item states
//	GET    /v1/batches/{id}/results   per-item outcomes
//	POST   /v1/batches/{id}/cancel    cancel pending items
func (s *Service) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/", s.handleSubmit)
	r.Get("/{id}", s.handleStatus)
	r.Get("/{id}/results", s.handleResults)
	r.Post("/{id}/cancel", s.handleCancel)
	return r
}

type submitRequest struct {
	Shape    string            `json:"shape"`    // inbound wire shape of each item (adapter name)
	Requests []json.RawMessage `json:"requests"` // one request body per item
}

func (s *Service) handleSubmit(w http.ResponseWriter, r *http.Request) {
	rk := inference.RelayKeyFromContext(r.Context())
	if rk == nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var req submitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Shape == "" {
		writeErr(w, http.StatusBadRequest, "shape is required")
		return
	}
	if len(req.Requests) == 0 {
		writeErr(w, http.StatusBadRequest, "requests must be non-empty")
		return
	}
	items := make([][]byte, len(req.Requests))
	for i, raw := range req.Requests {
		items[i] = raw
	}
	id, err := s.Submit(r.Context(), rk.Spec.KeyHash, rk.Spec.PolicyID, req.Shape, items)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": id, "status": string(StatusQueued), "total_items": len(items),
	})
}

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	rk := inference.RelayKeyFromContext(r.Context())
	if rk == nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	view, err := s.Status(r.Context(), chi.URLParam(r, "id"), rk.Spec.KeyHash)
	if err != nil {
		writeLookupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Service) handleResults(w http.ResponseWriter, r *http.Request) {
	rk := inference.RelayKeyFromContext(r.Context())
	if rk == nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	results, err := s.Results(r.Context(), chi.URLParam(r, "id"), rk.Spec.KeyHash)
	if err != nil {
		writeLookupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Service) handleCancel(w http.ResponseWriter, r *http.Request) {
	rk := inference.RelayKeyFromContext(r.Context())
	if rk == nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if err := s.Cancel(r.Context(), chi.URLParam(r, "id"), rk.Spec.KeyHash); err != nil {
		writeLookupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": string(StatusCancelled)})
}

// writeLookupErr maps store/ownership errors to HTTP. A non-owner gets 404
// (not 403) so a batch's existence isn't leaked to other keys.
func writeLookupErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		writeErr(w, http.StatusNotFound, "batch not found")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"type": "invalid_request_error", "message": msg},
	})
}
