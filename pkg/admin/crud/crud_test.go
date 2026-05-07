package crud_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/pkg/admin/crud"
	"github.com/wyolet/relay/internal/catalog"
)

// --- in-memory widget store ---

type widgetStore struct {
	mu      sync.Mutex
	widgets map[string]string // name -> value
}

func newWidgetStore(init ...string) *widgetStore {
	s := &widgetStore{widgets: make(map[string]string)}
	for _, v := range init {
		s.widgets[v] = v
	}
	return s
}

func (s *widgetStore) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.widgets))
	for _, v := range s.widgets {
		out = append(out, v)
	}
	return out
}

func (s *widgetStore) get(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.widgets[name]
	return v, ok
}

func (s *widgetStore) upsert(name, val string) {
	s.mu.Lock()
	s.widgets[name] = val
	s.mu.Unlock()
}

func (s *widgetStore) delete(name string) {
	s.mu.Lock()
	delete(s.widgets, name)
	s.mu.Unlock()
}

func (s *widgetStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.widgets)
}

// --- fake TxRunner ---

type fakeTxRunner struct {
	committed bool
	runErr    error
}

func (f *fakeTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if f.runErr != nil {
		return f.runErr
	}
	if err := fn(ctx); err != nil {
		return err
	}
	f.committed = true
	return nil
}

// --- fake Patcher ---

type fakePatcher struct {
	err error
}

func (f *fakePatcher) ValidateWithPatch(_ catalog.Patch) error { return f.err }

// --- fake Reloader ---

type fakeReloader struct {
	mu    sync.Mutex
	count int
	err   error
}

func (f *fakeReloader) Reload(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	return f.err
}

func (f *fakeReloader) reloadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

// --- audit capture ---

type auditHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *auditHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *auditHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r)
	h.mu.Unlock()
	return nil
}
func (h *auditHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *auditHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *auditHandler) last() (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) == 0 {
		return slog.Record{}, false
	}
	return h.records[len(h.records)-1], true
}

func (h *auditHandler) attrValue(rec slog.Record, key string) (any, bool) {
	var found any
	var ok bool
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found = a.Value.Any()
			ok = true
			return false
		}
		return true
	})
	return found, ok
}

// --- test Kind[string] ---

func buildKind(ws *widgetStore, insertErr, updateErr, deleteErr error) *crud.Kind[string] {
	return &crud.Kind[string]{
		Name: "Widget",
		Decode: func(r *http.Request) (string, error) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return "", err
			}
			var payload struct{ Value string }
			if err := json.Unmarshal(body, &payload); err != nil {
				return "", err
			}
			if payload.Value == "" {
				return "", errors.New("value required")
			}
			return payload.Value, nil
		},
		List: func(_ context.Context) ([]string, error) {
			return ws.list(), nil
		},
		Get: func(_ context.Context, name string) (string, error) {
			v, ok := ws.get(name)
			if !ok {
				return "", crud.ErrNotFound
			}
			return v, nil
		},
		Insert: func(_ context.Context, v string) error {
			if insertErr != nil {
				return insertErr
			}
			ws.upsert(v, v)
			return nil
		},
		Update: func(_ context.Context, name, v string) error {
			if updateErr != nil {
				return updateErr
			}
			ws.upsert(name, v)
			return nil
		},
		Delete: func(_ context.Context, name string) error {
			if deleteErr != nil {
				return deleteErr
			}
			ws.delete(name)
			return nil
		},
		ResourceID: func(v string) string { return v },
		Summarize:  func(before, after string) string { return before + "->" + after },
	}
}

func makeDeps(tx *fakeTxRunner, patcher *fakePatcher, reloader *fakeReloader, log *slog.Logger) crud.Deps {
	return crud.Deps{
		Tx:       tx,
		Patcher:  patcher,
		Reloader: reloader,
		Logger:   log,
	}
}

func errEnvelope(t *testing.T, body *strings.Reader) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return out
}

// --- tests ---

func TestList_ReturnsItems(t *testing.T) {
	t.Parallel()
	ws := newWidgetStore("a", "b")
	ah := &auditHandler{}
	k := buildKind(ws, nil, nil, nil)
	deps := makeDeps(&fakeTxRunner{}, &fakePatcher{}, &fakeReloader{}, slog.New(ah))
	list, _, _, _, _ := k.Handlers(deps)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/widgets", nil)
	list(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var out struct{ Items []string }
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 2 {
		t.Errorf("want 2 items, got %d", len(out.Items))
	}
}

func TestGet_Missing_404Envelope(t *testing.T) {
	t.Parallel()
	ws := newWidgetStore()
	ah := &auditHandler{}
	k := buildKind(ws, nil, nil, nil)
	deps := makeDeps(&fakeTxRunner{}, &fakePatcher{}, &fakeReloader{}, slog.New(ah))
	_, get, _, _, _ := k.Handlers(deps)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/admin/widgets/ghost", nil)
	get(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
	out := errEnvelope(t, strings.NewReader(w.Body.String()))
	if _, ok := out["error"]; !ok {
		t.Error("want error envelope")
	}
}

func TestCreate_ValidBody_201_ReloadAudit(t *testing.T) {
	t.Parallel()
	ws := newWidgetStore()
	ah := &auditHandler{}
	reloader := &fakeReloader{}
	tx := &fakeTxRunner{}
	k := buildKind(ws, nil, nil, nil)
	deps := makeDeps(tx, &fakePatcher{}, reloader, slog.New(ah))
	_, _, create, _, _ := k.Handlers(deps)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/widgets",
		strings.NewReader(`{"value":"foo"}`))
	r.Header.Set("Authorization", "Bearer test-token-xyz")
	create(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	if !tx.committed {
		t.Error("tx not committed")
	}
	if reloader.reloadCount() != 1 {
		t.Errorf("want 1 reload, got %d", reloader.reloadCount())
	}

	rec, ok := ah.last()
	if !ok {
		t.Fatal("no audit record")
	}
	if !strings.Contains(rec.Message, "create") {
		t.Errorf("audit msg %q should contain 'create'", rec.Message)
	}
	hash, hasHash := ah.attrValue(rec, "token_hash")
	if !hasHash {
		t.Error("audit missing token_hash")
	}
	if hs, ok := hash.(string); !ok || len(hs) != 12 {
		t.Errorf("token_hash = %v, want 12-char hex string", hash)
	}
}

func TestCreate_BrokenRef_ValidationFail_400_RollsBack(t *testing.T) {
	t.Parallel()
	ws := newWidgetStore()
	ah := &auditHandler{}
	tx := &fakeTxRunner{}
	patcher := &fakePatcher{err: errors.New("broken reference")}
	k := buildKind(ws, nil, nil, nil)
	k.Patch = func(v string) catalog.Patch { return catalog.Patch{} }
	deps := makeDeps(tx, patcher, &fakeReloader{}, slog.New(ah))
	_, _, create, _, _ := k.Handlers(deps)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/widgets",
		strings.NewReader(`{"value":"bad"}`))
	create(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	// Validation fails before RunInTx, so tx is not committed.
	if tx.committed {
		t.Error("tx should not be committed on validation failure")
	}
	if ws.len() != 0 {
		t.Error("store should be unchanged on validation failure")
	}
}

func TestCreate_MalformedJSON_400Envelope(t *testing.T) {
	t.Parallel()
	ws := newWidgetStore()
	ah := &auditHandler{}
	k := buildKind(ws, nil, nil, nil)
	deps := makeDeps(&fakeTxRunner{}, &fakePatcher{}, &fakeReloader{}, slog.New(ah))
	_, _, create, _, _ := k.Handlers(deps)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/widgets",
		strings.NewReader("not-json"))
	create(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	out := errEnvelope(t, strings.NewReader(w.Body.String()))
	if _, ok := out["error"]; !ok {
		t.Error("want error envelope")
	}
}

func TestUpdate_200_ReloadAudit(t *testing.T) {
	t.Parallel()
	ws := newWidgetStore("x")
	ah := &auditHandler{}
	reloader := &fakeReloader{}
	tx := &fakeTxRunner{}
	k := buildKind(ws, nil, nil, nil)
	deps := makeDeps(tx, &fakePatcher{}, reloader, slog.New(ah))
	_, _, _, update, _ := k.Handlers(deps)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/admin/widgets/x",
		strings.NewReader(`{"value":"x"}`))
	r.Header.Set("X-Relay-Admin-Token", "admintoken")
	update(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !tx.committed {
		t.Error("tx not committed")
	}
	if reloader.reloadCount() != 1 {
		t.Errorf("want 1 reload, got %d", reloader.reloadCount())
	}
	rec, ok := ah.last()
	if !ok {
		t.Fatal("no audit record")
	}
	if !strings.Contains(rec.Message, "update") {
		t.Errorf("audit msg %q should contain 'update'", rec.Message)
	}
}

func TestDelete_204_ReloadAudit(t *testing.T) {
	t.Parallel()
	ws := newWidgetStore("y")
	ah := &auditHandler{}
	reloader := &fakeReloader{}
	tx := &fakeTxRunner{}
	k := buildKind(ws, nil, nil, nil)
	deps := makeDeps(tx, &fakePatcher{}, reloader, slog.New(ah))
	_, _, _, _, del := k.Handlers(deps)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/admin/widgets/y", nil)
	del(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", w.Code, w.Body.String())
	}
	if !tx.committed {
		t.Error("tx not committed")
	}
	if reloader.reloadCount() != 1 {
		t.Errorf("want 1 reload, got %d", reloader.reloadCount())
	}
	rec, ok := ah.last()
	if !ok {
		t.Fatal("no audit record")
	}
	if !strings.Contains(rec.Message, "delete") {
		t.Errorf("audit msg %q should contain 'delete'", rec.Message)
	}
}
