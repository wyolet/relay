// POST /apply uses the same loader as the boot seed (app/apply), so a CI
// apply and a boot seed of the same tree converge on the same rows.

package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/refcheck"
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

// applyMu serializes applies within this process. Plan reads every row and
// Execute writes them back, so two overlapping runs interleave reads and
// writes over the same rows; per-write re-reads (see apply.Execute) catch
// what crosses pods, this catches the common single-pod case up front.
var applyMu sync.Mutex

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
		if d.Stores == nil {
			return nil, huma.Error500InternalServerError("stores not wired")
		}
		docs, err := parseBundle(in.ContentType, in.RawBody)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		applyMu.Lock()
		defer applyMu.Unlock()
		plan, err := apply.Plan(ctx, docs, apply.Options{
			Stores:   applyStores(d),
			Force:    in.Force,
			Prune:    in.Prune,
			Selector: in.Selector,
			Gov:      d.Catalog,
			License:  applyLicense(d),
			Authz:    d.Authz,
		})
		if err != nil {
			var ge *apply.GovernanceError
			if errors.As(err, &ge) {
				return nil, huma.Error403Forbidden(ge.Error())
			}
			if errors.Is(err, license.ErrRequired) || errors.Is(err, authz.ErrForbidden) {
				return nil, huma.Error403Forbidden(err.Error())
			}
			// A reference the caller may not see answers like any refused row:
			// generic, so the plan step cannot probe another scope's names.
			var re *refcheck.Error
			if errors.As(err, &re) && re.Status == http.StatusNotFound {
				return nil, &applyFailure{status: http.StatusForbidden, Message: "forbidden"}
			}
			return nil, huma.Error400BadRequest(err.Error())
		}

		// Every write needs system.apply at its scope plus the row's own verb;
		// a dry run runs the same pass so a caller who may write nothing never
		// gets the diff back.
		if err := apply.Authorize(ctx, plan, d.Authz); err != nil {
			var ae *apply.AuthzError
			if errors.As(err, &ae) {
				status, msg := http.StatusForbidden, "forbidden"
				if errors.Is(err, authz.ErrUnauthenticated) {
					status, msg = http.StatusUnauthorized, "unauthorized"
				}
				// Naming the refused row would report state the caller may
				// not see; the plan is already withheld for that reason.
				return nil, &applyFailure{status: status, Message: msg}
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}

		out := &applyOutput{}
		out.Body.Plan = plan.Entries
		out.Body.Counts = plan.Counts
		if in.DryRun {
			audit.Discard(ctx)
			return out, nil
		}

		applied, err := apply.Execute(ctx, plan, d.Authz)
		if err != nil {
			var se *apply.StoreError
			if errors.As(err, &se) {
				recordApplied(ctx, d, se.Applied, &se.Entry)
				return nil, &applyFailure{
					status: http.StatusInternalServerError, Message: se.Error(),
					Plan: plan.Entries, Applied: se.Applied,
				}
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		recordApplied(ctx, d, applied, nil)
		// Execute may have downgraded entries to conflict; report the plan
		// as it actually ran.
		out.Body.Plan = plan.Entries
		out.Body.Counts = plan.Counts
		out.Body.Applied = true
		return out, nil
	})
}

// recordApplied audits each change that landed as its own row, plus the
// write that failed, if any. Nothing landed leaves the request's single row.
func recordApplied(ctx context.Context, d Deps, applied []apply.Entry, failed *apply.Entry) {
	var snap *appcatalog.Snapshot
	if d.Catalog != nil {
		snap = d.Catalog.Current()
	}
	row := func(e apply.Entry, status string) audit.Row {
		action, kind, owner := e.Authorized()
		fields := e.ChangedFields
		if e.Action != apply.ActionUpdate {
			fields = []string{audit.AnyField}
		}
		return audit.Row{
			Action: action, Status: status, Fields: fields,
			Resource: audit.Resource{Kind: kind, ID: e.ID, Name: e.Name, Owner: &owner, Scope: audit.ScopeOf(snap, &owner)},
		}
	}
	rows := make([]audit.Row, 0, len(applied)+1)
	for _, e := range applied {
		rows = append(rows, row(e, audit.StatusAllowed))
	}
	if failed != nil {
		rows = append(rows, row(*failed, audit.StatusError))
	}
	if len(rows) > 0 {
		audit.RecordEach(ctx, rows)
	}
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

// applyLicense is the gate a bundle posted to the API is checked against.
// A deployment with no licence service is a community deployment, not an
// ungated one — apply.Options treats nil as "no gate".
func applyLicense(d Deps) license.Checker {
	if d.License == nil {
		return license.Community
	}
	return d.License
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
