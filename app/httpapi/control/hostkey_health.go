package control

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/keypool"
)

// GET /host-keys/by-id/{id}/health — read the per-key circuit-breaker state so
// an operator can see why a key is being skipped (auth-failed → indefinite
// open, rate-limited / server-error → timed cooldown) without grepping logs or
// poking Redis. Read-only; reports the stored record verbatim.
type hostKeyHealthInput struct {
	ID string `path:"id" doc:"HostKey id (UUIDv7)."`
}

type hostKeyHealth struct {
	State                    string     `json:"state" doc:"closed | open | half_open | unknown."`
	Reason                   string     `json:"reason,omitempty" doc:"Why the circuit opened (cooldown reason), when open."`
	Indefinite               bool       `json:"indefinite,omitempty" doc:"Open with no expiry (auth failure) until the key heals or rotates."`
	OpenUntil                *time.Time `json:"open_until,omitempty" doc:"When a timed cooldown expires; absent when closed or indefinite."`
	CooldownRemainingSeconds *int       `json:"cooldown_remaining_seconds,omitempty" doc:"Seconds until a timed cooldown expires; absent when closed, indefinite, or already elapsed."`
	BackoffStep              int        `json:"backoff_step" doc:"Position on the server-error backoff ladder (0 = healthy)."`
	LastTransition           *time.Time `json:"last_transition,omitempty" doc:"Last state change; doubles as last-success time when state is closed."`
	HasRecord                bool       `json:"has_record" doc:"False when no breaker record exists yet — the key has never failed (assumed healthy)."`
}

type hostKeyHealthResponse struct {
	Body hostKeyHealth `json:"body"`
}

func registerHostKeyHealth(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "get_host_key_health",
		Method:      http.MethodGet,
		Path:        "/host-keys/by-id/{id}/health",
		Summary:     "Read a HostKey's circuit-breaker state",
		Tags:        []string{"host-keys"},
		Middlewares: protect,
		Errors:      []int{401, 404, 500},
	}, func(ctx context.Context, in *hostKeyHealthInput) (*hostKeyHealthResponse, error) {
		if err := d.Authz.Authorize(ctx, "host-keys.read", authz.Resource{Kind: "host-key", ID: in.ID}); err != nil {
			return nil, mapAuthzErr(err)
		}
		if d.Selector == nil {
			return nil, huma.Error500InternalServerError("circuit-breaker state is unavailable")
		}
		existing, err := d.Stores.HostKey.Get(ctx, in.ID)
		if err != nil || existing == nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("host-key %q not found", in.ID))
		}

		// KeyHash is derived from the resolved secret value. When the secret
		// can't be resolved (env var absent, decrypt failed) it's empty and no
		// breaker record is reachable — report state unknown rather than lie.
		if existing.KeyHash == "" {
			return &hostKeyHealthResponse{Body: hostKeyHealth{State: "unknown"}}, nil
		}

		rec, found := d.Selector.ReadCircuit(ctx, existing.KeyHash)
		out := hostKeyHealth{
			State:       rec.State.String(),
			Reason:      string(rec.Reason),
			Indefinite:  rec.Indefinite,
			BackoffStep: rec.BackoffStep,
			HasRecord:   found,
		}
		if !rec.LastTransition.IsZero() {
			lt := rec.LastTransition
			out.LastTransition = &lt
		}
		if rec.State == keypool.CircuitOpen && !rec.Indefinite && !rec.OpenUntil.IsZero() {
			ou := rec.OpenUntil
			out.OpenUntil = &ou
			if remain := int(time.Until(ou).Seconds()); remain > 0 {
				out.CooldownRemainingSeconds = &remain
			}
		}
		return &hostKeyHealthResponse{Body: out}, nil
	})
}
