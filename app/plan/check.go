// Checking a set of rows before it is written: the rows are laid over a published view's input rows, a scratch engine runs every plan controller on the result, and the issues the result has that the view does not are returned. The scratch engine has its own cell registry and no hooks, and is closed before the answer, so nothing the request path reads changes.
//
// Out of scope: reading storage, and deciding what a caller is shown of an issue.

package plan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/engine/cell"
	"github.com/wyolet/relay/pkg/meta"
)

// ErrCheckTimeout is a check that did not finish within its bound, the wait for an earlier check included.
var ErrCheckTimeout = errors.New("plan: the check did not finish in time")

// Check runs proposed rows through the plan controllers, one check at a time.
type Check struct {
	rbac    bool
	timeout time.Duration
	slot    chan struct{}
}

// NewCheck returns a Check that builds its scratch plans as the live ones are built under rbac (Sources.RBAC), and gives up after timeout.
func NewCheck(rbac bool, timeout time.Duration) *Check {
	return &Check{rbac: rbac, timeout: timeout, slot: make(chan struct{}, 1)}
}

// NewIssues lays proposed over live's input rows (each a pointer to an input row such as *account.Account; one with an id live holds replaces that row), runs every controller on the result, and returns the issues of the result that live does not have, warnings left out. Two issues are the same when their severity, code, source, target and field are. Past the timeout, the wait for an earlier check included, it returns ErrCheckTimeout.
func (c *Check) NewIssues(ctx context.Context, live *engine.View, proposed []any) ([]engine.Issue, error) {
	if live == nil {
		return nil, errors.New("plan: no published view to check against")
	}
	src, err := layered(live, proposed)
	if err != nil {
		return nil, err
	}
	src.RBAC = c.rbac
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	select {
	case c.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, checkStopped(ctx)
	}
	type built struct {
		v   *engine.View
		err error
	}
	done := make(chan built, 1)
	go func() {
		defer func() { <-c.slot }()
		v, err := scratchView(src)
		done <- built{v, err}
	}()
	select {
	case b := <-done:
		if b.err != nil {
			return nil, b.err
		}
		return newIssues(live.AllIssues(), b.v.AllIssues()), nil
	case <-ctx.Done():
		return nil, checkStopped(ctx)
	}
}

func checkStopped(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrCheckTimeout
	}
	return ctx.Err()
}

// scratchView loads an engine of its own over src and returns its first view once the engine is closed.
func scratchView(src Sources) (*engine.View, error) {
	cells := cell.NewRegistry(cell.Options{})
	e := engine.New(engine.Options{Cells: cells})
	Register(e, cells, src)
	err := e.Load(context.Background())
	v := e.View()
	_ = e.Close()
	if err != nil {
		return nil, fmt.Errorf("plan: check: %w", err)
	}
	return v, nil
}

// issueKey is what makes two issues the same one; the message is wording.
type issueKey struct {
	severity       engine.Severity
	code           string
	source, target engine.Ref
	field          string
}

func keyOfIssue(i engine.Issue) issueKey {
	return issueKey{severity: i.Severity, code: i.Code, source: i.Source, target: i.Target, field: i.Field}
}

// newIssues returns the issues of after that before does not have, warnings left out.
func newIssues(before, after []engine.Issue) []engine.Issue {
	had := make(map[issueKey]bool, len(before))
	for _, i := range before {
		had[keyOfIssue(i)] = true
	}
	var out []engine.Issue
	for _, i := range after {
		if i.Severity != engine.SeverityWarning && !had[keyOfIssue(i)] {
			out = append(out, i)
		}
	}
	return out
}

// layered is a Sources serving live's input rows with proposed laid over them; a proposed row of no input kind is an error.
func layered(live *engine.View, proposed []any) (Sources, error) {
	used := 0
	byMeta := func(md meta.Metadata) string { return md.ID }
	src := Sources{
		Providers:       layer(live, proposed, &used, func(r *provider.Provider) string { return byMeta(r.Meta) }),
		Hosts:           layer(live, proposed, &used, func(r *host.Host) string { return byMeta(r.Meta) }),
		Models:          layer(live, proposed, &used, func(r *model.Model) string { return byMeta(r.Meta) }),
		Bindings:        layer(live, proposed, &used, func(r *binding.Binding) string { return byMeta(r.Meta) }),
		Pricings:        layer(live, proposed, &used, func(r *pricing.Pricing) string { return byMeta(r.Meta) }),
		Overlays:        layer(live, proposed, &used, func(r *overlay.Overlay) string { return OverlayID(r.Kind, r.ResourceID) }),
		Policies:        layer(live, proposed, &used, func(r *policy.Policy) string { return byMeta(r.Meta) }),
		PolicyBindings:  layer(live, proposed, &used, func(r *policybinding.PolicyBinding) string { return byMeta(r.Meta) }),
		RateLimits:      layer(live, proposed, &used, func(r *ratelimit.RateLimit) string { return byMeta(r.Meta) }),
		Budgets:         layer(live, proposed, &used, func(r *budget.Budget) string { return byMeta(r.Meta) }),
		Pools:           layer(live, proposed, &used, func(r *pool.Pool) string { return byMeta(r.Meta) }),
		Accounts:        layer(live, proposed, &used, func(r *account.Account) string { return byMeta(r.Meta) }),
		Credentials:     layer(live, proposed, &used, func(r *hostkey.HostKey) string { return byMeta(r.Meta) }),
		Teams:           layer(live, proposed, &used, func(r *team.Team) string { return byMeta(r.Meta) }),
		Projects:        layer(live, proposed, &used, func(r *project.Project) string { return byMeta(r.Meta) }),
		ServiceAccounts: layer(live, proposed, &used, func(r *serviceaccount.ServiceAccount) string { return byMeta(r.Meta) }),
		Users:           layer(live, proposed, &used, func(r *user.User) string { return r.ID }),
		Keys:            layer(live, proposed, &used, func(r *key.Key) string { return byMeta(r.Meta) }),
		Roles:           layer(live, proposed, &used, func(r *role.Role) string { return byMeta(r.Meta) }),
		RoleBindings:    layer(live, proposed, &used, func(r *rolebinding.RoleBinding) string { return byMeta(r.Meta) }),
		Groups:          layer(live, proposed, &used, func(r *group.Group) string { return byMeta(r.Meta) }),
		AccountsSetting: layer(live, proposed, &used, func(*settings.Accounts) string { return settings.SectionAccounts }),
	}
	if used != len(proposed) {
		return Sources{}, fmt.Errorf("plan: check: %d of %d proposed rows are of no input kind", len(proposed)-used, len(proposed))
	}
	return src, nil
}

// layer is live's rows of kind T with the proposed rows of that kind laid over them, counting the proposed rows it took in used.
func layer[T any](live *engine.View, proposed []any, used *int, id func(*T) string) rowList[T] {
	var mine []*T
	replaced := map[string]bool{}
	for _, p := range proposed {
		if r, ok := p.(*T); ok && r != nil {
			mine = append(mine, r)
			replaced[id(r)] = true
			*used++
		}
	}
	out := rowList[T]{id: id}
	for r := range engine.All[T](live) {
		if !replaced[id(r)] {
			out.rows = append(out.rows, r)
		}
	}
	out.rows = append(out.rows, mine...)
	return out
}

// rowList is a source over rows held in memory.
type rowList[T any] struct {
	rows []*T
	id   func(*T) string
}

func (l rowList[T]) List(context.Context) ([]*T, error) { return l.rows, nil }

func (l rowList[T]) Get(_ context.Context, id string) (*T, error) {
	for _, r := range l.rows {
		if l.id(r) == id {
			return r, nil
		}
	}
	return nil, nil
}
