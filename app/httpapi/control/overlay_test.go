package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wyolet/relay/app/catalogstore"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/meta"
)

// overlayDBTX backs the overlay delete of one provider-owned model: every model read finds it, and the first delete removes the overlays it holds.
type overlayDBTX struct {
	execOnlyDBTX
	overlays int64
}

func (db *overlayDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	n := db.overlays
	db.overlays = 0
	return pgconn.NewCommandTag("DELETE " + strconv.FormatInt(n, 10)), nil
}

func (db *overlayDBTX) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	return modelRow{id: args[0].(string)}
}

type modelRow struct{ id string }

func (r modelRow) Scan(dest ...any) error {
	*dest[0].(*string) = r.id
	*dest[1].(*string) = "acme-large"
	*dest[3].(*[]byte) = []byte(`{"owner":{"kind":"provider","id":"acme"}}`)
	*dest[4].(*[]byte) = []byte(`{}`)
	return nil
}

func TestOverlayDeleteReportsWhetherOneExisted(t *testing.T) {
	deps := mountDeps(t)
	q := gen.New(&overlayDBTX{overlays: 1})
	deps.Stores.Model, deps.Stores.Overlay = catalogstore.NewModelStore(q), overlay.NewStore(q)
	r := chi.NewRouter()
	Mount(r, deps)

	for i, want := range []bool{true, false} {
		req := httptest.NewRequest(http.MethodDelete, "/models/by-id/"+meta.NewID()+"/overlay", nil)
		req.Header.Set("Authorization", "Bearer "+deps.AdminToken)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("delete %d: status = %d, want 200: %s", i+1, w.Code, w.Body)
		}
		var got struct{ Deleted *bool }
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Deleted == nil || *got.Deleted != want {
			t.Errorf("delete %d: body %s, want deleted %v", i+1, w.Body, want)
		}
	}
}

func TestBuildOverlayView(t *testing.T) {
	spec := func() model.Spec {
		return model.Spec{Snapshots: []model.Snapshot{{Name: "claude-fable-5"}}, Pointer: "claude-fable-5", Aliases: []string{"fable"}}
	}
	id := meta.NewID()
	tmpl := &model.Model{Meta: meta.Metadata{ID: id, Name: "claude-fable-5", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: meta.NewID()}}, Spec: spec()}
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	merged := spec()
	merged.Family, merged.Aliases = "custom", []string{"fable", "nick"}

	for _, tc := range []struct {
		name  string
		patch string
		want  overlayView
	}{
		{name: "no overlay serves the template", want: overlayView{Effective: spec()}},
		{name: "a patch merges into effective", patch: `{"aliases":["nick"],"family":"custom"}`, want: overlayView{Effective: merged}},
		{name: "a patch that no longer merges is quarantined", patch: `{"pointer":"nope"}`, want: overlayView{Effective: spec(), Quarantined: true, QuarantineReason: `overlay: effective row invalid: model "claude-fable-5": pointer "nope" does not match any snapshot`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o *overlay.Overlay
			tc.want.Kind, tc.want.ResourceID, tc.want.Template = "model", id, spec()
			if tc.patch != "" {
				o = &overlay.Overlay{Kind: overlay.KindModel, ResourceID: id, Patch: json.RawMessage(tc.patch), UpdatedAt: at}
				tc.want.Patch, tc.want.UpdatedAt = json.RawMessage(tc.patch), at
			}
			if diff := cmp.Diff(tc.want, buildOverlayView(tmpl, o)); diff != "" {
				t.Errorf("view (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(spec(), tmpl.Spec); diff != "" {
				t.Errorf("the merge changed the template (-want +got):\n%s", diff)
			}
		})
	}
}
