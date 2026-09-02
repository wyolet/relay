package inference

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/wyolet/relay/app/adapters"
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
			// The credential was checked once, at the upgrade; a connection
			// then lives for hours. Re-run the revocation checks per frame
			// against the live snapshot so a revoked key or token stops
			// working without waiting for the client to reconnect.
			if p := PrincipalFrom(fr.Context()); p != nil && d.Catalog != nil {
				if err := p.Recheck(d.Catalog.Current(), time.Now()); err != nil {
					writeAuthErr(fw, err.Error())
					return
				}
			}
			handleShape(spec, d, fw, fr)
		}

		_ = transportws.Serve(r.Context(), conn, r, perFrame, transportws.Options{
			Logger: slog.Default(),
		})
	}
}
