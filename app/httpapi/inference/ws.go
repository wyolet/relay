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
			// The upgrade pinned the snapshot its credential resolved
			// against, and a connection then lives for hours: inheriting
			// that pin would serve every later frame from a catalog frozen
			// at handshake time. Each frame re-pins the live snapshot, so a
			// frame is still internally consistent but never stale.
			var snap *appcatalog.Snapshot
			if d.Catalog != nil {
				snap = d.Catalog.Current()
			}
			fr = fr.WithContext(WithSnapshot(fr.Context(), snap))
			// The credential was checked once, at the upgrade. Re-run the
			// revocation checks per frame so a revoked key or token stops
			// working without waiting for the client to reconnect, and
			// re-resolve the policy against this frame's snapshot — a
			// rebound policy binding must reach a live connection too.
			// Frames run concurrently, so the re-resolution writes to a
			// copy and never to the connection's shared principal.
			if p := PrincipalFrom(fr.Context()); p != nil && snap != nil {
				if err := p.Recheck(snap, time.Now()); err != nil {
					writeAuthErr(fw, err.Error())
					return
				}
				frame := framePrincipal(p, snap)
				if !resolvePolicy(fw, snap, frame) {
					return
				}
				fr = fr.WithContext(context.WithValue(fr.Context(), ctxPrincipalT{}, frame))
			}
			handleShape(spec, d, fw, fr)
		}

		_ = transportws.Serve(r.Context(), conn, r, perFrame, transportws.Options{
			Logger: slog.Default(),
		})
	}
}

// framePrincipal copies the connection's principal and re-reads what the
// credential resolves to in snap: the key row (and with it the policy it
// names) may have been rebound since the upgrade. The copy is what keeps
// concurrent frames from writing the connection's shared principal.
func framePrincipal(p *Principal, snap *appcatalog.Snapshot) *Principal {
	frame := *p
	frame.Policy = nil
	if frame.CredentialKind == CredentialKey {
		if k, _ := snap.KeyByHash(frame.KeyHash); k != nil {
			frame.Key = k
		}
	}
	if frame.Key != nil && frame.Key.Spec.PolicyID != "" {
		if pol, ok := snap.Policy(frame.Key.Spec.PolicyID); ok {
			frame.Policy = pol
		}
	}
	return &frame
}
