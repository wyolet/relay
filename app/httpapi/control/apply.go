// apply.go serves POST /apply: a manifest bundle is diffed against the
// stored rows and, unless dryRun, written. The loader is app/apply — the
// same one the boot seed runs — so a CI apply and a boot seed of the same
// tree converge on the same rows.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/manifest"
)

type applyInput struct {
	ContentType string `header:"Content-Type" doc:"application/yaml (multi-document) or application/json ({\"documents\": [...]})."`
	DryRun      bool   `query:"dryRun"        doc:"Plan only; write nothing."`
	Force       bool   `query:"force"         doc:"Write over operator-edited (dirty) rows."`
	Prune       bool   `query:"prune"         doc:"Delete selected rows the bundle omits. Requires selector."`
	Selector    string `query:"selector"      doc:"Label selector naming the managed set, e.g. env=prod,team=platform."`
	RawBody     []byte
}

type applyOutput struct {
	Body struct {
		Plan    []apply.Entry `json:"plan"`
		Applied bool          `json:"applied" doc:"False for a dry run."`
		Counts  apply.Counts  `json:"counts"`
	}
}

// applyFailure carries the plan (and, for a partial write, what landed)
// alongside the status. It implements huma.StatusError so the body the
// client sees is this value, not the generic error model.
type applyFailure struct {
	status  int
	Message string        `json:"message"`
	Plan    []apply.Entry `json:"plan"`
	Applied []apply.Entry `json:"applied,omitempty"`
}

func (e *applyFailure) Error() string  { return e.Message }
func (e *applyFailure) GetStatus() int { return e.status }

func registerApply(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "apply",
		Method:      http.MethodPost,
		Path:        "/apply",
		Summary:     "Apply a manifest bundle",
		Description: "Diffs the submitted documents against the stored rows and writes the difference. " +
			"Every row is authorized before anything is written; a denied row fails the whole apply.",
		Tags:        []string{"system"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 500},
	}, func(ctx context.Context, in *applyInput) (*applyOutput, error) {
		if err := d.Authz.Authorize(ctx, "system.apply", authz.Resource{Kind: "system"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		if d.Stores == nil {
			return nil, huma.Error500InternalServerError("stores not wired")
		}
		docs, err := parseBundle(in.ContentType, in.RawBody)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		plan, err := apply.Plan(ctx, docs, apply.Options{
			Stores:   applyStores(d),
			Force:    in.Force,
			Prune:    in.Prune,
			Selector: in.Selector,
		})
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}

		out := &applyOutput{}
		out.Body.Plan = plan.Entries
		out.Body.Counts = plan.Counts
		if in.DryRun {
			return out, nil
		}

		applied, err := apply.Execute(ctx, plan, d.Authz)
		if err != nil {
			var ae *apply.AuthzError
			if errors.As(err, &ae) {
				status := http.StatusForbidden
				if errors.Is(err, authz.ErrUnauthenticated) {
					status = http.StatusUnauthorized
				}
				return nil, &applyFailure{status: status, Message: ae.Error(), Plan: plan.Entries}
			}
			var se *apply.StoreError
			if errors.As(err, &se) {
				return nil, &applyFailure{
					status: http.StatusInternalServerError, Message: se.Error(),
					Plan: plan.Entries, Applied: se.Applied,
				}
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		_ = applied
		out.Body.Applied = true
		return out, nil
	})
}

// parseBundle reads a multi-document YAML body, or a JSON envelope of the
// same documents. JSON is valid YAML flow syntax, so the JSON documents are
// concatenated into one YAML stream and both shapes share a parser.
func parseBundle(contentType string, raw []byte) ([]manifest.Document, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("empty request body")
	}
	if strings.Contains(contentType, "json") {
		var env struct {
			Documents []json.RawMessage `json:"documents"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		for i, d := range env.Documents {
			if i > 0 {
				buf.WriteString("\n---\n")
			}
			buf.Write(d)
		}
		raw = buf.Bytes()
	}
	return manifest.Parse(bytes.NewReader(raw))
}

// applyStores narrows the control plane's store bundle to what the loader
// needs. app/apply declares its own set: app/catalog (which owns Deps.Stores)
// sits above the boot seed and cannot be imported from below it.
func applyStores(d Deps) *apply.Stores {
	return &apply.Stores{
		Provider:    d.Stores.Provider,
		Host:        d.Stores.Host,
		RateLimit:   d.Stores.RateLimit,
		HostKey:     d.Stores.HostKey,
		Model:       d.Stores.Model,
		Policy:      d.Stores.Policy,
		Pricing:     d.Stores.Pricing,
		HostBinding: d.Stores.Binding,
		Key:         d.Stores.Key,
		Team:        d.Stores.Team,
		Project:     d.Stores.Project,

		ServiceAccount: d.Stores.ServiceAccount,
		Group:          d.Stores.Group,
		Role:           d.Stores.Role,
		RoleBinding:    d.Stores.RoleBinding,
		PolicyBinding:  d.Stores.PolicyBinding,
		Overlay:        d.Stores.Overlay,

		User: d.Users,
	}
}
