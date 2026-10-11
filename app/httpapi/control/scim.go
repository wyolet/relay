// /scim/v2 is the auth module's SCIM service provider, mounted at the control listener's root because identity providers are configured with one fixed base URL. The auth:scim section switches it on and picks the delete behaviour per request, so a settings change needs no restart.

package control

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/scim"
)

// SCIMPrefix is where the SCIM base URL lives on the control listener.
const SCIMPrefix = "/scim/v2"

// MountSCIM installs /scim/v2 on r, the control listener's root router. It answers 404 while auth:scim is disabled, and is not mounted at all without the stores and the catalog it reads its settings from.
func MountSCIM(r chi.Router, d Deps) {
	if d.Stores == nil || d.Stores.SCIMClients == nil || d.Catalog == nil {
		return
	}
	dir, err := newDirectoryOps(d).directory()
	if err != nil {
		slog.Error("relay control: SCIM not mounted", "err", err)
		return
	}
	base := ""
	if d.PublicURL != "" {
		base = strings.TrimRight(d.PublicURL, "/") + SCIMPrefix
	}
	cfg := scim.Config{Directory: dir, Tokens: d.Stores.SCIMClients, BaseURL: base, OnWrite: collectSCIMWrite}
	deleting, err := scim.New(cfg)
	if err != nil {
		slog.Error("relay control: SCIM not mounted", "err", err)
		return
	}
	cfg.DeleteDisables = true
	disabling, err := scim.New(cfg)
	if err != nil {
		slog.Error("relay control: SCIM not mounted", "err", err)
		return
	}
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		section := settings.AuthSCIMFrom(d.Catalog)
		if !section.Enabled {
			writeSCIMDisabled(w)
			return
		}
		var next http.Handler = deleting
		if section.DeleteDisables {
			next = disabling
		}
		auditSCIM(next).ServeHTTP(w, r)
	})
	h = http.StripPrefix(SCIMPrefix, h)
	if d.Audit != nil {
		h = audit.Middleware(d.Audit, d.TrustedProxies)(h)
	}
	r.Handle(SCIMPrefix, h)
	r.Handle(SCIMPrefix+"/*", h)
}

func writeSCIMDisabled(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:Error"],"status":"404","detail":"SCIM provisioning is disabled"}` + "\n"))
}

// scimWrites gathers a request's writes, so the audit row names the SCIM client as the actor and lists every change the request made.
type scimWrites struct {
	mu     sync.Mutex
	client string
	rows   []audit.Row
}

type scimWritesKey struct{}

func auditSCIM(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := &scimWrites{}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), scimWritesKey{}, c)))
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.rows) == 0 {
			return
		}
		first := c.rows[0]
		audit.Record(r.Context(), first.Action, first.Resource, first.Status, audit.Actor{Kind: audit.ActorSCIMClient, Name: c.client})
		audit.RecordEach(r.Context(), c.rows)
	})
}

// collectSCIMWrite turns a SCIM write into an audit row: allowed when it ran, denied when the client reached for a row it did not provision. A write that failed validation changed nothing and records nothing.
func collectSCIMWrite(ctx context.Context, w scim.Write) {
	c, ok := ctx.Value(scimWritesKey{}).(*scimWrites)
	if !ok {
		return
	}
	status := audit.StatusAllowed
	switch {
	case w.Err == nil:
	case errors.Is(w.Err, directory.ErrForeign):
		status = audit.StatusDenied
	default:
		return
	}
	action, fields := w.Op, w.Fields
	switch w.Op {
	case "create", "delete":
		fields = []string{audit.AnyField}
	case "disable", "enable":
		action, fields = "update", []string{"disabled"}
	}
	owner := meta.Owner{Kind: meta.OwnerSystem}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = w.Client
	c.rows = append(c.rows, audit.Row{
		Action:   w.Kind + "s." + action,
		Resource: audit.Resource{Kind: w.Kind, ID: w.ID, Name: w.Name, Owner: &owner},
		Status:   status,
		Fields:   fields,
	})
}
