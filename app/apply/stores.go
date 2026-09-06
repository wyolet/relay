package apply

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	appsecret "github.com/wyolet/relay/app/secret"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Stores bundles every store a plan reads or writes. Declared here rather
// than reused from app/catalog: app/catalog depends on the boot seed, which
// depends on this package.
type Stores struct {
	Provider    *provider.Store
	Host        *host.Store
	RateLimit   *ratelimit.Store
	HostKey     *hostkey.Store
	Model       *model.Store
	Policy      *policy.Store
	Pricing     *pricing.Store
	HostBinding *binding.Store
	Key         *key.Store
	Team        *team.Store
	Project     *project.Store

	ServiceAccount *serviceaccount.Store
	Group          *group.Store
	Role           *role.Store
	RoleBinding    *rolebinding.Store
	PolicyBinding  *policybinding.Store
	Overlay        *overlay.Store

	// User is read-only here: users are not a manifest kind, but group
	// members and user-principal keys name them, so the resolver needs
	// their slugs.
	User *user.Store
}

// NewStores wires every store against pool. masterKey enables stored-mode
// HostKey rows; nil disables them.
func NewStores(pool *pgxpool.Pool, masterKey []byte) *Stores {
	q := gen.New(pool)
	secReg, secStored := appsecret.Wire(q, pool, masterKey)
	return &Stores{
		Provider:    provider.NewStore(q),
		Host:        host.NewStore(q),
		RateLimit:   ratelimit.NewStore(q),
		HostKey:     hostkey.NewStore(q, secReg, secStored),
		Model:       model.NewStore(q),
		Policy:      policy.NewStore(pool),
		Pricing:     pricing.NewStore(pool),
		HostBinding: binding.NewStore(pool),
		Key:         key.NewStore(q),
		Team:        team.NewStore(q),
		Project:     project.NewStore(q),

		ServiceAccount: serviceaccount.NewStore(q),
		Group:          group.NewStore(pool),
		Role:           role.NewStore(q),
		RoleBinding:    rolebinding.NewStore(pool),
		PolicyBinding:  policybinding.NewStore(pool),
		Overlay:        overlay.NewStore(q),

		User: user.NewStore(q),
	}
}

// Rows is every existing row of every kind, loaded once so planning,
// pruning, and export share a single sweep of Postgres.
type Rows struct {
	Providers  []*provider.Provider
	Hosts      []*host.Host
	RateLimits []*ratelimit.RateLimit
	HostKeys   []*hostkey.HostKey
	Models     []*model.Model
	Pricings   []*pricing.Pricing
	Bindings   []*binding.Binding
	Policies   []*policy.Policy
	Keys       []*key.Key
	Teams      []*team.Team
	Projects   []*project.Project

	ServiceAccounts []*serviceaccount.ServiceAccount
	Groups          []*group.Group
	Roles           []*role.Role
	RoleBindings    []*rolebinding.RoleBinding
	PolicyBindings  []*policybinding.PolicyBinding
	Overlays        []*overlay.Overlay

	Users []*user.User
}

// rowState is the provenance one planned write is conditional on: the row
// either was absent, or carried exactly this (dirty, updatedAt) pair when
// the plan was built.
type rowState struct {
	present   bool
	dirty     bool
	updatedAt time.Time
}

// rowStates indexes every loaded row's provenance by kind and name, so
// Execute can tell whether the rows it is about to write still look the way
// Plan saw them.
func rowStates(r *Rows) map[string]map[string]rowState {
	out := make(map[string]map[string]rowState, 17)
	add := func(kind string, m *meta.Metadata) {
		byName, ok := out[kind]
		if !ok {
			byName = map[string]rowState{}
			out[kind] = byName
		}
		byName[m.Name] = rowState{present: true, dirty: m.Dirty, updatedAt: m.UpdatedAt}
	}
	for _, x := range r.Providers {
		add("Provider", &x.Meta)
	}
	for _, x := range r.Hosts {
		add("Host", &x.Meta)
	}
	for _, x := range r.RateLimits {
		add("RateLimit", &x.Meta)
	}
	for _, x := range r.HostKeys {
		add("HostKey", &x.Meta)
	}
	for _, x := range r.Models {
		add("Model", &x.Meta)
	}
	for _, x := range r.Pricings {
		add("Pricing", &x.Meta)
	}
	for _, x := range r.Bindings {
		add("HostBinding", &x.Meta)
	}
	for _, x := range r.Policies {
		add("Policy", &x.Meta)
	}
	for _, x := range r.Keys {
		add("Key", &x.Meta)
	}
	for _, x := range r.Teams {
		add("Team", &x.Meta)
	}
	for _, x := range r.Projects {
		add("Project", &x.Meta)
	}
	for _, x := range r.ServiceAccounts {
		add("ServiceAccount", &x.Meta)
	}
	for _, x := range r.Groups {
		add("Group", &x.Meta)
	}
	for _, x := range r.Roles {
		add("Role", &x.Meta)
	}
	for _, x := range r.RoleBindings {
		add("RoleBinding", &x.Meta)
	}
	for _, x := range r.PolicyBindings {
		add("PolicyBinding", &x.Meta)
	}
	return out
}

// Load lists every kind once.
func Load(ctx context.Context, s *Stores) (*Rows, error) {
	r := &Rows{}
	var err error
	if r.Teams, err = s.Team.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list teams: %w", err)
	}
	if r.Projects, err = s.Project.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list projects: %w", err)
	}
	if r.Providers, err = s.Provider.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list providers: %w", err)
	}
	if r.Hosts, err = s.Host.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list hosts: %w", err)
	}
	if r.RateLimits, err = s.RateLimit.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list ratelimits: %w", err)
	}
	if r.HostKeys, err = s.HostKey.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list hostkeys: %w", err)
	}
	if r.Models, err = s.Model.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list models: %w", err)
	}
	if r.Pricings, err = s.Pricing.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list pricings: %w", err)
	}
	if r.Bindings, err = s.HostBinding.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list hostbindings: %w", err)
	}
	if r.Policies, err = s.Policy.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list policies: %w", err)
	}
	if r.Keys, err = s.Key.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list keys: %w", err)
	}
	if r.ServiceAccounts, err = s.ServiceAccount.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list serviceaccounts: %w", err)
	}
	if r.Groups, err = s.Group.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list groups: %w", err)
	}
	if r.Roles, err = s.Role.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list roles: %w", err)
	}
	if r.RoleBindings, err = s.RoleBinding.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list rolebindings: %w", err)
	}
	if r.PolicyBindings, err = s.PolicyBinding.List(ctx); err != nil {
		return nil, fmt.Errorf("apply: list policybindings: %w", err)
	}
	if s.Overlay != nil {
		if r.Overlays, err = s.Overlay.List(ctx); err != nil {
			return nil, fmt.Errorf("apply: list overlays: %w", err)
		}
	}
	if s.User != nil {
		if r.Users, err = s.User.List(ctx); err != nil {
			return nil, fmt.Errorf("apply: list users: %w", err)
		}
	}
	return r, nil
}
