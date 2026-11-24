// Checking a set of rows before it is written: the rows are laid over a published view's input rows, with the rows the writer read from the database in between, a scratch engine runs every plan controller on the result, and the issues the result has that the baseline does not are returned. The scratch engine has its own cell registry and no hooks, and is closed before the answer, so nothing the request path reads changes.
//
// Out of scope: reading storage, and deciding what a caller is shown of an issue.

package plan

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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

// NewIssues lays read, then proposed, over live's input rows (each a pointer to an input row such as *account.Account; a later row with an id already laid replaces it), runs every controller on the result, and returns the issues of the result that the baseline does not have, warnings left out. read is what the database holds now of rows the caller read, which live may not have seen yet; a read row of no input kind is left out. The baseline is live's issues when every read row is the one live holds, else the issues of live with read laid over it, built the same way, so a row live has not seen yet neither hides nor causes an issue. Two issues are the same when their severity, code, source, target and field are. Past the timeout, the wait for an earlier check included, it returns ErrCheckTimeout.
func (c *Check) NewIssues(ctx context.Context, live *engine.View, read, proposed []any) ([]engine.Issue, error) {
	if live == nil {
		return nil, errors.New("plan: no published view to check against")
	}
	full, stale, err := layered(live, read, proposed)
	if err != nil {
		return nil, err
	}
	full.RBAC = c.rbac
	var base *Sources
	if stale {
		b, _, _ := layered(live, read, nil)
		b.RBAC = c.rbac
		base = &b
	}
	held := ctx.Value(heldKey{c}) != nil
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if !held {
		select {
		case c.slot <- struct{}{}:
		case <-ctx.Done():
			return nil, checkStopped(ctx)
		}
	}
	type built struct {
		before, after []engine.Issue
		err           error
	}
	done := make(chan built, 1)
	go func() {
		if !held {
			defer func() { <-c.slot }()
		}
		var b built
		b.before = live.AllIssues()
		if base != nil {
			v, err := scratchView(*base)
			if err != nil {
				done <- built{err: err}
				return
			}
			b.before = v.AllIssues()
		}
		v, err := scratchView(full)
		if err != nil {
			done <- built{err: err}
			return
		}
		b.after = v.AllIssues()
		done <- b
	}()
	select {
	case b := <-done:
		if b.err != nil {
			return nil, b.err
		}
		return newIssues(b.before, b.after), nil
	case <-ctx.Done():
		return nil, checkStopped(ctx)
	}
}

// heldKey marks a context whose caller holds c's slot (Hold).
type heldKey struct{ c *Check }

// Hold waits for the check slot, at most the bound and while ctx lives, and returns ctx marked as holding it with the function that frees it; a check made under that ctx runs in the slot without taking it again. A caller that writes what it checks holds the slot across its whole write, so one such write is open at a time and the others wait holding nothing. Past the bound it returns ErrCheckTimeout.
func (c *Check) Hold(ctx context.Context) (context.Context, func(), error) {
	wait, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	select {
	case c.slot <- struct{}{}:
	case <-wait.Done():
		return ctx, func() {}, checkStopped(wait)
	}
	return context.WithValue(ctx, heldKey{c}, true), func() { <-c.slot }, nil
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

// layering lays read, then proposed, over live's rows, one kind at a time.
type layering struct {
	live           *engine.View
	read, proposed []any
	// used counts the proposed rows some kind took; stale is set by a read row live does not hold as it is.
	used  int
	stale bool
}

// layered is a Sources serving live's input rows with read, then proposed, laid over them, and whether a read row differs from live's; a proposed row of no input kind is an error.
func layered(live *engine.View, read, proposed []any) (Sources, bool, error) {
	l := &layering{live: live, read: read, proposed: proposed}
	byMeta := func(md meta.Metadata) string { return md.ID }
	src := Sources{
		Providers:       layer(l, func(r *provider.Provider) string { return byMeta(r.Meta) }),
		Hosts:           layer(l, func(r *host.Host) string { return byMeta(r.Meta) }),
		Models:          layer(l, func(r *model.Model) string { return byMeta(r.Meta) }),
		Bindings:        layer(l, func(r *binding.Binding) string { return byMeta(r.Meta) }),
		Pricings:        layer(l, func(r *pricing.Pricing) string { return byMeta(r.Meta) }),
		Overlays:        layer(l, func(r *overlay.Overlay) string { return OverlayID(r.Kind, r.ResourceID) }),
		Policies:        layer(l, func(r *policy.Policy) string { return byMeta(r.Meta) }),
		PolicyBindings:  layer(l, func(r *policybinding.PolicyBinding) string { return byMeta(r.Meta) }),
		RateLimits:      layer(l, func(r *ratelimit.RateLimit) string { return byMeta(r.Meta) }),
		Budgets:         layer(l, func(r *budget.Budget) string { return byMeta(r.Meta) }),
		Pools:           layer(l, func(r *pool.Pool) string { return byMeta(r.Meta) }),
		Accounts:        layer(l, func(r *account.Account) string { return byMeta(r.Meta) }),
		Credentials:     layer(l, func(r *hostkey.HostKey) string { return byMeta(r.Meta) }),
		Teams:           layer(l, func(r *team.Team) string { return byMeta(r.Meta) }),
		Projects:        layer(l, func(r *project.Project) string { return byMeta(r.Meta) }),
		ServiceAccounts: layer(l, func(r *serviceaccount.ServiceAccount) string { return byMeta(r.Meta) }),
		Users:           layer(l, func(r *user.User) string { return r.ID }),
		Keys:            layer(l, func(r *key.Key) string { return byMeta(r.Meta) }),
		Roles:           layer(l, func(r *role.Role) string { return byMeta(r.Meta) }),
		RoleBindings:    layer(l, func(r *rolebinding.RoleBinding) string { return byMeta(r.Meta) }),
		Groups:          layer(l, func(r *group.Group) string { return byMeta(r.Meta) }),
		AccountsSetting: layer(l, func(*settings.Accounts) string { return settings.SectionAccounts }),
	}
	if l.used != len(proposed) {
		return Sources{}, false, fmt.Errorf("plan: check: %d of %d proposed rows are of no input kind", len(proposed)-l.used, len(proposed))
	}
	return src, l.stale, nil
}

// layer is live's rows of kind T with l's read rows, then its proposed rows, of that kind laid over them; a later row with an id already laid replaces it in place.
func layer[T any](l *layering, id func(*T) string) rowList[T] {
	out := rowList[T]{id: id}
	at := map[string]int{}
	put := func(r *T) {
		if i, ok := at[id(r)]; ok {
			out.rows[i] = r
			return
		}
		at[id(r)] = len(out.rows)
		out.rows = append(out.rows, r)
	}
	for r := range engine.All[T](l.live) {
		put(r)
	}
	for _, x := range l.read {
		if r, ok := x.(*T); ok && r != nil {
			if held := engine.Get[T](l.live, id(r)); held == nil || !reflect.DeepEqual(held, r) {
				l.stale = true
			}
			put(r)
		}
	}
	for _, x := range l.proposed {
		if r, ok := x.(*T); ok && r != nil {
			l.used++
			put(r)
		}
	}
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
