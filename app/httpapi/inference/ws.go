package inference

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/wyolet/relay/app/adapters"
	appcatalog "github.com/wyolet/relay/app/catalog"
	transportws "github.com/wyolet/relay/app/transport/ws"
	"github.com/wyolet/relay/pkg/httpmw"
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
			fr, ok := frameRequest(d, fw, fr)
			if !ok {
				return
			}
			handleShape(spec, d, fw, fr)
		}

		_ = transportws.Serve(r.Context(), conn, r, perFrame, transportws.Options{
			ReadLimit: wsReadLimit(d.MaxRequestBytes),
			// Each frame is its own request and must not share the upgrade's id.
			PerRequest: func(ctx context.Context) context.Context { return reqid.WithNewID(ctx, slog.Default()) },
			Logger:     slog.Default(),
		})
	}
}

// frameRequest gives one frame its own request state: the live snapshot and the credential re-resolved against it. Frames run concurrently over the upgrade's context, so the state is a copy, never the upgrade's. Reports false after writing the rejection.
func frameRequest(d Deps, fw http.ResponseWriter, fr *http.Request) (*http.Request, bool) {
	// A connection lives for hours, so each frame re-pins the live
	// snapshot instead of inheriting the handshake's: consistent
	// within a frame, never stale across frames.
	var snap *appcatalog.Snapshot
	if d.Catalog != nil {
		snap = d.Catalog.Current()
	}
	frame := derivedState(fr.Context())
	frame.snap, frame.snapSet = snap, true
	ctx := withState(fr.Context(), frame)
	// Authentication re-runs per frame against the current plans, so a
	// revoked or rotated credential, a rebound policy or a disabled
	// subject reaches a live connection. Frames run concurrently, so each
	// gets its own principal.
	if p := PrincipalFrom(ctx); p != nil {
		fp, ok := d.Auth().reauthenticate(fw, p)
		if !ok {
			return nil, false
		}
		if !authorizePrincipal(fw, fp, frame.cls.Mode) {
			return nil, false
		}
		frame.principal = fp
	}
	// The upgrade was admitted under the proxy-mode setting of its time; a change since must reach the live connection too.
	if !authorizeProxyMode(fw, d.Catalog, frame.cls.Mode) {
		return nil, false
	}
	return fr.WithContext(ctx), true
}

// wsFrameEnvelopeBytes is the room a frame's id and field names take on top of the request body it carries.
const wsFrameEnvelopeBytes = 4 << 10

// wsReadLimit caps an inbound frame at the HTTP body limit plus its envelope, so a request the HTTP routes accept also fits in a frame.
func wsReadLimit(maxBody int64) int64 {
	if maxBody <= 0 {
		maxBody = httpmw.DefaultMaxRequestBytes
	}
	return maxBody + wsFrameEnvelopeBytes
}

// Recheck resolves a credential admitted earlier (the submitter of a queued batch item) against the current plans: a key by the hash it presented, a token by its user's version, its expiry and the revoked set. The error says why it no longer authenticates; the caller ladder's own refusal is the caller's to check.
func (a *Auth) Recheck(keyHash, userID, jti string, tokenVer int, tokenExp int64, now time.Time) (*Principal, error) {
	root := a.root()
	if root == nil {
		return nil, errors.New("plans not loaded")
	}
	if keyHash != "" {
		kp := root.Keys.Lookup(keyHash, now)
		if kp == nil {
			return nil, errors.New("api key no longer valid")
		}
		return principalOf(kp, keyHash), nil
	}
	switch up := root.Users.Lookup(userID); {
	case tokenExp > 0 && tokenExp <= now.Unix():
		return nil, errors.New("token expired")
	case up == nil || up.TokenVersion != tokenVer:
		return nil, errors.New(msgTokenRevoked)
	case a.Revoked.Revoked(jti, now):
		return nil, errors.New("token revoked")
	default:
		return &Principal{UserID: userID, CredentialKind: CredentialToken, CredentialID: jti, TokenExp: tokenExp, TokenVer: tokenVer, Caller: up.Caller}, nil
	}
}

// reauthenticate resolves an admitted principal's credential again against the current plans, without the bearer: a key by the hash it presented, a token by the claims it verified to. Reports false after writing the response.
func (a *Auth) reauthenticate(w http.ResponseWriter, p *Principal) (*Principal, bool) {
	root := a.root()
	if root == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "not_ready", "relay is starting; retry shortly")
		return nil, false
	}
	now := time.Now()
	if p.CredentialKind == CredentialToken {
		return tokenClaimsPrincipal(w, a, root, p.claims, p.claimsTyp, now)
	}
	kp := root.Keys.Lookup(p.KeyHash, now)
	if kp == nil {
		writeAuthErr(w, "invalid api key")
		return nil, false
	}
	return principalOf(kp, p.KeyHash), true
}
