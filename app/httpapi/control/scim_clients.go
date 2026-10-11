package control

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/scim"
)

type scimClientRow struct {
	Name        string     `json:"name"                 doc:"The client's name, stamped as the source of every user and group it provisions."`
	TokenPrefix string     `json:"tokenPrefix"          doc:"The start of the token, for telling tokens apart."`
	CreatedAt   time.Time  `json:"createdAt"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty" doc:"When the token last authenticated, to the minute."`
	Revoked     bool       `json:"revoked"`
}

func scimClientRowOf(c *scim.Client) scimClientRow {
	row := scimClientRow{Name: c.Name, TokenPrefix: c.TokenPrefix, CreatedAt: c.CreatedAt, Revoked: c.Revoked}
	if !c.LastUsedAt.IsZero() {
		t := c.LastUsedAt
		row.LastUsedAt = &t
	}
	return row
}

type scimClientListOutput struct {
	Body struct {
		Clients []scimClientRow `json:"clients"`
	}
}

type scimClientCreateInput struct {
	Body struct {
		Name string `json:"name" doc:"A DNS-1123 label naming the identity provider, such as entra or okta. Issuing a revoked client's name again hands its users and groups to the new token." maxLength:"63"`
	}
}

type scimClientCreateOutput struct {
	Body struct {
		scimClientRow
		Token string `json:"token" doc:"The bearer token the identity provider sends. Returned once; only its hash is kept."`
	}
}

type scimClientRevokeInput struct {
	Name string `path:"name"`
}

// scimClientResource is where scim-clients actions are authorized: the global scope, as only an administrator may hand out provisioning rights.
func scimClientResource(name string) authz.Resource {
	owner := meta.Owner{Kind: meta.OwnerSystem}
	return authz.Resource{Kind: "scim-client", Name: name, Owner: &owner}
}

func registerSCIMClients(api huma.API, d Deps, protect huma.Middlewares) {
	if d.Stores == nil || d.Stores.SCIMClients == nil {
		return
	}
	store := d.Stores.SCIMClients
	huma.Register(api, huma.Operation{
		OperationID: "scim_clients_list",
		Method:      http.MethodGet,
		Path:        "/scim-clients",
		Summary:     "List SCIM clients",
		Tags:        []string{"scim"},
		Middlewares: protect,
		Errors:      []int{401, 403, 500},
	}, func(ctx context.Context, _ *struct{}) (*scimClientListOutput, error) {
		if err := d.Authz.Authorize(ctx, "scim-clients.list", scimClientResource("")); err != nil {
			return nil, mapAuthzErr(err)
		}
		clients, err := store.ListClients(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := &scimClientListOutput{}
		out.Body.Clients = make([]scimClientRow, len(clients))
		for i, c := range clients {
			out.Body.Clients[i] = scimClientRowOf(c)
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "scim_clients_create",
		Method:        http.MethodPost,
		Path:          "/scim-clients",
		Summary:       "Create a SCIM client and return its token once",
		Tags:          []string{"scim"},
		Middlewares:   protect,
		DefaultStatus: http.StatusCreated,
		Errors:        []int{400, 401, 403, 409, 500},
	}, func(ctx context.Context, in *scimClientCreateInput) (*scimClientCreateOutput, error) {
		if err := d.Authz.Authorize(ctx, "scim-clients.create", scimClientResource(in.Body.Name)); err != nil {
			return nil, mapAuthzErr(err)
		}
		if !directory.ValidName(in.Body.Name) {
			return nil, huma.Error400BadRequest("name must be a DNS-1123 label (lowercase letters, digits and dashes)")
		}
		token, c, err := scim.CreateClient(ctx, store, in.Body.Name)
		if errors.Is(err, scim.ErrClientExists) {
			return nil, huma.Error409Conflict("a live SCIM client already has this name; revoke it first to issue a new token")
		}
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		audit.Changed(ctx, []string{audit.AnyField})
		created, err := clientNamed(ctx, store, c.Name)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := &scimClientCreateOutput{}
		out.Body.scimClientRow, out.Body.Token = scimClientRowOf(created), token
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "scim_clients_revoke",
		Method:      http.MethodPost,
		Path:        "/scim-clients/{name}/revoke",
		Summary:     "Revoke a SCIM client's token",
		Description: "The token stops authenticating at once. The users and groups the client provisioned stay, still stamped with its name.",
		Tags:        []string{"scim"},
		Middlewares: protect,
		Errors:      []int{401, 403, 404, 500},
	}, func(ctx context.Context, in *scimClientRevokeInput) (*emptyOutput, error) {
		res := scimClientResource(in.Name)
		if err := d.Authz.Authorize(ctx, "scim-clients.revoke", res); err != nil {
			return nil, mapAuthzErr(err)
		}
		err := store.RevokeClient(ctx, in.Name)
		if errors.Is(err, scim.ErrNoClient) {
			return nil, huma.Error404NotFound("no SCIM client " + in.Name)
		}
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		audit.Record(ctx, "scim-clients.revoke", audit.Resource{Kind: res.Kind, Name: in.Name, Owner: res.Owner}, audit.StatusAllowed)
		return &emptyOutput{}, nil
	})
}

// clientNamed reads a client back so a create answers with the stored timestamps.
func clientNamed(ctx context.Context, store scim.TokenStore, name string) (*scim.Client, error) {
	clients, err := store.ListClients(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range clients {
		if c.Name == name {
			return c, nil
		}
	}
	return nil, errors.New("scim client " + name + " vanished after create")
}
