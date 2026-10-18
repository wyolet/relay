// /secret-keys manages the key ring stored secrets are sealed under: data keys per owner scope, wrapped by the master key. Responses carry key ids and metadata, never key bytes. The relay secret CLI runs the same operations against Postgres when the control plane is down.

package control

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/secret/keyring"
)

// SecretKeyManager runs the key ring operations; *stored.Backend implements it.
type SecretKeyManager interface {
	Status(ctx context.Context) (stored.KeyStatus, error)
	Rotate(ctx context.Context, scope string) (stored.DataKeyStatus, error)
	// RotateAll returns the keys it made even when it fails part-way, so they can be audited.
	RotateAll(ctx context.Context) ([]stored.DataKeyStatus, error)
	Rewrap(ctx context.Context, to string) (stored.RewrapResult, error)
	Destroy(ctx context.Context, id string, force bool) (stored.DataKeyStatus, error)
}

type secretKeysStatusOutput struct {
	Body stored.KeyStatus
}

type secretKeysRotateInput struct {
	Body struct {
		Scope string `json:"scope,omitempty" maxLength:"256" doc:"The scope to rotate, as listed by GET /secret-keys (system, user:<id>, project:<id>)."`
		All   bool   `json:"all,omitempty" doc:"Rotate every scope that has a data key. Exclusive with scope."`
	}
}

type secretKeysRotateOutput struct {
	Body struct {
		Keys []stored.DataKeyStatus `json:"keys" doc:"The new Primary data keys."`
	}
}

type secretKeysRewrapInput struct {
	Body struct {
		To string `json:"to" maxLength:"64" doc:"The KEK id to wrap every data key under, from heldKeks in GET /secret-keys."`
	}
}

type secretKeysRewrapOutput struct {
	Body stored.RewrapResult
}

type secretKeyDestroyBody struct {
	Force bool `json:"force,omitempty" doc:"Skip the wait after demotion, for a compromised key. A Primary or a key in use is still refused."`
}

type secretKeyDestroyInput struct {
	ID   string                `path:"id" doc:"Data key id (UUID)."`
	Body *secretKeyDestroyBody `json:"body,omitempty" required:"false"`
}

type secretKeyDestroyOutput struct {
	Body stored.DataKeyStatus
}

// secretKeyResource is where secret-keys actions are authorized: the global scope, as the key ring belongs to the deployment.
func secretKeyResource(id, name string) authz.Resource {
	owner := meta.Owner{Kind: meta.OwnerSystem}
	return authz.Resource{Kind: "secret-key", ID: id, Name: name, Owner: &owner}
}

// authorizeSecretKeys checks the RBAC verb, then requires an admin: the key ring guards every stored credential, so a custom role holding the verb must not be enough.
func authorizeSecretKeys(ctx context.Context, a authz.Authorizer, action string, res authz.Resource) error {
	if err := a.Authorize(ctx, action, res); err != nil {
		return mapAuthzErr(err)
	}
	if err := refcheck.RequireAdmin(ctx, a, "managing secret keys"); err != nil {
		return huma.Error403Forbidden(err.Error())
	}
	return nil
}

func secretKeyRow(action string, k stored.DataKeyStatus, fields ...string) audit.Row {
	owner := meta.Owner{Kind: meta.OwnerSystem}
	return audit.Row{
		Action:   action,
		Resource: audit.Resource{Kind: "secret-key", ID: k.ID, Name: k.Scope, Owner: &owner},
		Fields:   fields,
	}
}

// recordPartialRotation audits the keys a failed rotate-all did make, as succeeded, beside the request's own row with the error status, so the audit log matches the key ring.
func recordPartialRotation(ctx context.Context, rotated []stored.DataKeyStatus, name string) {
	if len(rotated) == 0 {
		return
	}
	owner := meta.Owner{Kind: meta.OwnerSystem}
	rows := make([]audit.Row, 0, len(rotated)+1)
	for _, k := range rotated {
		row := secretKeyRow("secret-keys.rotate", k)
		row.Code = http.StatusOK
		rows = append(rows, row)
	}
	rows = append(rows, audit.Row{Action: "secret-keys.rotate", Resource: audit.Resource{Kind: "secret-key", Name: name, Owner: &owner}})
	audit.RecordEach(ctx, rows)
}

func registerSecretKeys(api huma.API, d Deps, protect huma.Middlewares) {
	if d.SecretKeys == nil {
		return
	}
	keys := d.SecretKeys
	huma.Register(api, huma.Operation{
		OperationID: "secret_keys_status",
		Method:      http.MethodGet,
		Path:        "/secret-keys",
		Summary:     "List the data keys stored secrets are sealed under",
		Description: "Data keys with their scope, state, wrapping KEK and row count; the target KEK; the KEKs this pod holds; and the stored secrets still in the legacy master-key shape. Never returns key bytes.",
		Tags:        []string{"secret-keys"},
		Middlewares: protect,
		Errors:      []int{401, 403, 500, 503},
	}, func(ctx context.Context, _ *struct{}) (*secretKeysStatusOutput, error) {
		if err := authorizeSecretKeys(ctx, d.Authz, "secret-keys.list", secretKeyResource("", "")); err != nil {
			return nil, err
		}
		st, err := keys.Status(ctx)
		if err != nil {
			return nil, secretKeyError(err)
		}
		if st.DataKeys == nil {
			st.DataKeys = []stored.DataKeyStatus{}
		}
		return &secretKeysStatusOutput{Body: st}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "secret_keys_rotate",
		Method:      http.MethodPost,
		Path:        "/secret-keys/rotate",
		Summary:     "Rotate a scope's data key, or every scope's",
		Description: "The new key becomes the scope's Primary and the old one decrypt-only; values under the old key stay readable and are rewritten under the new one in the background. Body: {\"scope\": \"...\"} or {\"all\": true}.",
		Tags:        []string{"secret-keys"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 404, 409, 500, 503},
	}, func(ctx context.Context, in *secretKeysRotateInput) (*secretKeysRotateOutput, error) {
		name := in.Body.Scope
		if in.Body.All {
			name = "*"
		}
		if err := authorizeSecretKeys(ctx, d.Authz, "secret-keys.rotate", secretKeyResource("", name)); err != nil {
			return nil, err
		}
		if in.Body.All == (in.Body.Scope != "") {
			return nil, huma.Error400BadRequest("give either scope or all: true")
		}
		var rotated []stored.DataKeyStatus
		if in.Body.All {
			var err error
			if rotated, err = keys.RotateAll(ctx); err != nil {
				recordPartialRotation(ctx, rotated, name)
				return nil, secretKeyError(err)
			}
		} else {
			k, err := keys.Rotate(ctx, in.Body.Scope)
			if err != nil {
				return nil, secretKeyError(err)
			}
			rotated = []stored.DataKeyStatus{k}
		}
		rows := make([]audit.Row, len(rotated))
		for i, k := range rotated {
			rows[i] = secretKeyRow("secret-keys.rotate", k)
		}
		// Rotating every scope when none has a key yet changes nothing; the request's own row still records the attempt.
		if len(rows) > 0 {
			audit.RecordEach(ctx, rows)
		}
		out := &secretKeysRotateOutput{}
		out.Body.Keys = rotated
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "secret_keys_rewrap",
		Method:      http.MethodPost,
		Path:        "/secret-keys/rewrap",
		Summary:     "Wrap every data key under another master key",
		Description: "Makes the KEK the target and rewraps every data key under it. Refused (409) unless this pod holds that KEK, and while stored secrets remain in the legacy master-key shape. Data keys this pod cannot unwrap are listed in skipped; rerun on a pod holding both their KEK and the target.",
		Tags:        []string{"secret-keys"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 409, 500, 503},
	}, func(ctx context.Context, in *secretKeysRewrapInput) (*secretKeysRewrapOutput, error) {
		if err := authorizeSecretKeys(ctx, d.Authz, "secret-keys.rewrap", secretKeyResource("", in.Body.To)); err != nil {
			return nil, err
		}
		res, err := keys.Rewrap(ctx, in.Body.To)
		if err != nil {
			return nil, secretKeyError(err)
		}
		return &secretKeysRewrapOutput{Body: res}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "secret_keys_destroy",
		Method:      http.MethodPost,
		Path:        "/secret-keys/by-id/{id}/destroy",
		Summary:     "Destroy a demoted data key",
		Description: "Refused (409) for a scope's Primary, while stored secrets still use the key, and within 24h of its demotion unless force is set. The key's row stays as a tombstone, so a value found under it later fails as destroyed.",
		Tags:        []string{"secret-keys"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 404, 409, 500, 503},
	}, func(ctx context.Context, in *secretKeyDestroyInput) (*secretKeyDestroyOutput, error) {
		if err := authorizeSecretKeys(ctx, d.Authz, "secret-keys.destroy", secretKeyResource(in.ID, "")); err != nil {
			return nil, err
		}
		force := in.Body != nil && in.Body.Force
		k, err := keys.Destroy(ctx, in.ID, force)
		if err != nil {
			return nil, secretKeyError(err)
		}
		fields := []string{"state"}
		if force {
			fields = append(fields, "force")
		}
		audit.RecordEach(ctx, []audit.Row{secretKeyRow("secret-keys.destroy", k, fields...)})
		return &secretKeyDestroyOutput{Body: k}, nil
	})
}

// secretKeyError maps the key ring's refusals onto HTTP: a refusal the operator can act on is a 409 that says why.
func secretKeyError(err error) error {
	msg := err.Error()
	switch {
	case errors.Is(err, stored.ErrBadKeyID):
		return huma.Error400BadRequest(msg)
	case errors.Is(err, keyring.ErrUnknownKey), errors.Is(err, stored.ErrNoScopeKey):
		return huma.Error404NotFound(msg)
	case errors.Is(err, stored.ErrNoMasterKey):
		return huma.Error503ServiceUnavailable(msg)
	case errors.Is(err, keyring.ErrIsPrimary), errors.Is(err, keyring.ErrInUse), errors.Is(err, keyring.ErrTooRecent),
		errors.Is(err, keyring.ErrTargetKEKUnavailable), errors.Is(err, keyring.ErrKEKUnavailable),
		errors.Is(err, keyring.ErrConflict), errors.Is(err, stored.ErrLegacyRows):
		return huma.Error409Conflict(msg)
	}
	return huma.Error500InternalServerError(msg)
}
