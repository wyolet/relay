package control

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalogprune"
)

type catalogPruneInput struct {
	DryRun bool   `query:"dryRun" default:"true" doc:"true (the default) only decides; false deletes the rows decided prune."`
	Idle   string `query:"idle" doc:"How long a model or host binding must have served no request, as a Go duration or a day count (\"30d\"). Empty uses RELAY_CATALOG_PRUNE_IDLE."`
}

type catalogPruneOutput struct {
	Body struct {
		DryRun       bool                    `json:"dryRun"       doc:"Whether this was a dry run, which deletes nothing."`
		Idle         string                  `json:"idle"         doc:"The traffic window the rows were judged by, as a Go duration."`
		Pruned       int                     `json:"pruned"       doc:"Rows deleted, or that a dry run would delete."`
		KeptDetached int                     `json:"keptDetached" doc:"Rows that stay detached; the next prune judges them again."`
		Rows         []catalogprune.Decision `json:"rows"         doc:"Every detached catalog row with its decision."`
	}
}

func registerCatalogPrune(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "catalog_prune",
		Method:      http.MethodPost,
		Path:        "/catalog/prune",
		Summary:     "Delete detached catalog rows nothing holds",
		Description: "Judges every catalog row a catalog release stopped shipping (metadata.catalogDetachedAt set). " +
			"A row is pruned when it is not edited, has no overlay, is referenced by nothing but detached rows pruned with it, " +
			"and, for a model or host binding, served no request inside the idle window. Every other row stays detached. " +
			"A dry run (the default) only answers the decisions; dryRun=false deletes, and answers 503 while another relay is seeding the catalog.",
		Tags:        []string{"system"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 500, 503},
	}, func(ctx context.Context, in *catalogPruneInput) (*catalogPruneOutput, error) {
		return pruneCatalog(ctx, d, in)
	})
}

func pruneCatalog(ctx context.Context, d Deps, in *catalogPruneInput) (*catalogPruneOutput, error) {
	if err := d.Authz.Authorize(ctx, "system.catalog-prune", authz.Resource{Kind: "system"}); err != nil {
		return nil, mapAuthzErr(err)
	}
	if d.CatalogPrune == nil {
		return nil, huma.Error503ServiceUnavailable("catalog prune is not available on this relay")
	}
	var idle time.Duration
	if raw := strings.TrimSpace(in.Idle); raw != "" {
		v, err := parseSpan(raw)
		if err != nil || v <= 0 {
			return nil, huma.Error400BadRequest("invalid `idle` value: " + raw + " (want a positive Go duration or a day count such as 30d)")
		}
		idle = v
	}
	rep, err := d.CatalogPrune(ctx, idle, !in.DryRun)
	if in.DryRun {
		audit.Discard(ctx)
	} else {
		recordPruned(ctx, d, rep.Rows)
	}
	if errors.Is(err, appcatalog.ErrSeedBusy) {
		return nil, huma.Error503ServiceUnavailable("another relay is seeding the catalog now; prune again once it is done")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("catalog prune failed: " + err.Error())
	}
	out := &catalogPruneOutput{}
	out.Body.DryRun = in.DryRun
	out.Body.Idle = rep.Idle.String()
	out.Body.Pruned = rep.Pruned
	out.Body.KeptDetached = rep.Kept
	out.Body.Rows = rep.Rows
	if out.Body.Rows == nil {
		out.Body.Rows = []catalogprune.Decision{}
	}
	return out, nil
}

// recordPruned audits each deleted row as its own delete, including the ones a
// prune that failed part-way had already made.
func recordPruned(ctx context.Context, d Deps, rows []catalogprune.Decision) {
	var changes []audit.Row
	for _, r := range rows {
		if r.Deleted {
			changes = append(changes, audit.Row{Action: authz.Plural(r.Kind) + ".delete", Fields: []string{audit.AnyField},
				Resource: d.auditResource(r.Kind, r.ID, r.Name, r.Owner)})
		}
	}
	if len(changes) > 0 {
		audit.RecordEach(ctx, changes)
	}
}
