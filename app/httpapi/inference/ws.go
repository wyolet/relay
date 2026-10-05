package inference

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/wyolet/relay/app/adapters"
	appcatalog "github.com/wyolet/relay/app/catalog"
	transportws "github.com/wyolet/relay/app/transport/ws"
	"github.com/wyolet/relay/pkg/reqid"
)

// wsHandler upgrades a /v1/ws request to a WebSocket and serves the
// canonical (pkg/relay/v1) inference shape over it, multiplexing many
// requests on one connection. Authentication + classification already
// happened on the upgrade request via the shared middleware chain, so
// every frame inherits the authed context — the handshake is paid once.
//
// Each frame is dispatched through the unchanged handleShape/Dispatch
// path via a synthetic ResponseWriter (app/transport/ws). The transport
// is shape-agnostic; this handler pins it to the canonical spec.
func wsHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Must resolve + reject before Accept hijacks the connection;
		// after upgrade we can no longer write an HTTP error.
		spec := d.Specs.Spec(adapters.Canonical)
		if spec == nil {
			WriteAPIError(w, http.StatusInternalServerError, "server_error", "no_spec",
				"canonical adapter not registered")
			return
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			// Accept has already written the failure response.
			slog.Warn("ws: accept failed", "err", err)
			return
		}

		perFrame := func(fw http.ResponseWriter, fr *http.Request) {
			// A connection lives for hours, so each frame re-pins the live
			// snapshot instead of inheriting the handshake's: consistent
			// within a frame, never stale across frames.
			var snap *appcatalog.Snapshot
			if d.Catalog != nil {
				snap = d.Catalog.Current()
			}
			fr = fr.WithContext(WithSnapshot(fr.Context(), snap))
			// Revocation and policy resolution re-run per frame so a revoked
			// credential or rebound policy reaches a live connection. Frames
			// run concurrently, so this writes to a copy of the principal.
			if p := PrincipalFrom(fr.Context()); p != nil && snap != nil {
				if err := p.Recheck(snap, time.Now()); err != nil {
					writeAuthErr(fw, err.Error())
					return
				}
				frame := framePrincipal(p, snap)
				if !authorizePrincipal(fw, snap, frame, ClassificationFrom(fr.Context()).Mode) {
					return
				}
				fr = fr.WithContext(context.WithValue(fr.Context(), ctxPrincipalT{}, frame))
			}
			handleShape(spec, d, fw, fr)
		}

		_ = transportws.Serve(r.Context(), conn, r, perFrame, transportws.Options{
			// Each frame is its own request and must not share the upgrade's id.
			PerRequest: func(ctx context.Context) context.Context { return reqid.WithNewID(ctx, slog.Default()) },
			Logger:     slog.Default(),
		})
	}
}

// framePrincipal re-resolves the connection's credential against the frame's
// snapshot the way the HTTP edge does, so every change the snapshot reflects
// (key flags and policy, service account, project, group membership) reaches
// the next frame. Each frame gets its own value: frames run concurrently.
func framePrincipal(p *Principal, snap *appcatalog.Snapshot) *Principal {
	if p.CredentialKind == CredentialKey {
		if k, _ := snap.KeyByHash(p.KeyHash); k != nil {
			return buildPrincipal(snap, k, p.KeyHash)
		}
	}
	frame := *p
	frame.Policy = nil
	if p.token != nil {
		frame.Subjects = p.token.subjectsIn(snap, p.UserID)
	}
	if proj, ok := snap.Project(p.ProjectID); ok {
		frame.TeamID = proj.Spec.TeamID
	}
	return &frame
}
