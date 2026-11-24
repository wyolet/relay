// Checking a set of rows before it is written: the rows are laid over a published view's input rows, with the rows the writer read from the database in between, a scratch engine runs every plan controller on the result, and the issues the result has that the baseline does not are returned. The scratch engine has its own cell registry and no hooks, and is closed before the answer, so nothing the request path reads changes.
//
// Out of scope: reading storage, and deciding what a caller is shown of an issue.

package plan

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
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

// ErrCheckTimeout is a check that did not get the slot, or whose build did not finish, within its bound; also every check asked for while a build that outlived its bound still runs.
var ErrCheckTimeout = errors.New("plan: the check did not finish in time")

// Check runs proposed rows through the plan controllers, one check at a time.
type Check struct {
	rbac    bool
	timeout time.Duration
	slot    chan struct{}
	// overrun is set while a build that outlived its bound still holds the slot: an engine load cannot be stopped, so the slot stays taken until it ends, and checks asked for meanwhile answer at once instead of queueing behind it.
	overrun atomic.Bool
	// build builds a scratch view; scratchView outside tests.
	build func(Sources) (*engine.View, error)
}

// NewCheck returns a Check that builds its scratch plans as the live ones are built under rbac (Sources.RBAC), and gives up after timeout, once waiting for the slot and once building.
func NewCheck(rbac bool, timeout time.Duration) *Check {
	return &Check{rbac: rbac, timeout: timeout, slot: make(chan struct{}, 1), build: scratchView}
}

// hold is one holding of the slot. Its holder frees it, unless a build under it was given up on: that build frees it when it ends.
type hold struct {
	c                          *Check
	mu                         sync.Mutex
	building, abandoned, freed bool
}

func (h *hold) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.freed {
		return
	}
	if h.building {
		h.abandoned = true
		return
	}
	h.freed = true
	<-h.c.slot
}

// built ends a build under h, freeing the slot when the build was given up on.
func (h *hold) built() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.building = false
	if h.abandoned && !h.freed {
		h.freed = true
		h.c.overrun.Store(false)
		<-h.c.slot
	}
}

// Deleted proposes removing Row, a pointer to an input row such as *pool.Pool: the result holds no row of its kind under its id, whatever live or read hold. The baseline, built without the proposed rows, keeps it.
type Deleted struct{ Row any }

// NewIssues lays read, then proposed, over live's input rows (each a pointer to an input row such as *account.Account; a later row with an id already laid replaces it; a Deleted removes one), runs every controller on the result, and returns what the result refuses that the baseline does not: errors filed (warnings left out), and lanes the proposed rows make or bind that the result refuses as a whole (refusedLanes). read is what the database holds now of rows the caller read, which live may not have seen yet; a read row of no input kind is left out. The baseline is live when every read row is the one live holds, else live with read laid over it, built the same way, so a row live has not seen yet neither hides nor causes a refusal; and before answering a refusal against live, the baseline is built the same way as the result, so a difference between live, built change by change, and a build of the same rows at once refuses nothing. Two issues are the same when their severity, code, source, target and field are. It takes the slot as Hold does unless ctx already holds it; a build that outlives the bound, or a ctx that ends first, answers ErrCheckTimeout or ctx's error while the build runs on, holding the slot, to its end.
func (c *Check) NewIssues(ctx context.Context, live *engine.View, read, proposed []any) ([]engine.Issue, error) {
	f, err := c.Find(ctx, live, read, proposed)
	return f.Errors, err
}

// Findings is what a check found against its baseline: Errors refuse the rows (NewIssues); Warnings are the warnings the result has that the baseline does not, which refuse nothing. Result is the view built from the proposed rows, for reading what they make of the plans.
type Findings struct {
	Errors, Warnings []engine.Issue
	Result           *engine.View
	baseline         func() (*engine.View, error)
	within           func(func(context.Context) error) error
}

// Within runs work under the check's slot and bound, as the check's own builds run: past the bound, or once the ctx Find was given ends, it answers ErrCheckTimeout or that ctx's error, and the ctx work is given ends then too, so work reading it stops early.
func (f Findings) Within(work func(ctx context.Context) error) error {
	if f.within == nil {
		return errors.New("plan: the check built no result")
	}
	return f.within(work)
}

// Baseline is the view of the same rows without the proposed ones, built at once as Result was, never the live view; it is the one the check built when it needed it, else built on the first call under the check's bound and the ctx Find was given, as Find's own build: past the bound it answers ErrCheckTimeout and the slot stays taken until the build ends. The caller holds the check's slot, as a writer running the check does.
func (f Findings) Baseline() (*engine.View, error) {
	if f.baseline == nil {
		return nil, errors.New("plan: the check built no result")
	}
	return f.baseline()
}

// onceView builds a view once, or answers the one already built.
func onceView(built *engine.View, build func() (*engine.View, error)) func() (*engine.View, error) {
	var once sync.Once
	var err error
	return func() (*engine.View, error) {
		once.Do(func() {
			if built == nil {
				built, err = build()
			}
		})
		return built, err
	}
}

// Find is NewIssues that also answers the new warnings, taken from the same builds.
func (c *Check) Find(ctx context.Context, live *engine.View, read, proposed []any) (Findings, error) {
	if live == nil {
		return Findings{}, errors.New("plan: no published view to check against")
	}
	full, stale, err := layered(live, read, proposed)
	if err != nil {
		return Findings{}, err
	}
	full.RBAC = c.rbac
	base, _, _ := layered(live, read, nil)
	base.RBAC = c.rbac
	h, _ := ctx.Value(heldKey{c}).(*hold)
	if h == nil {
		held, release, err := c.Hold(ctx)
		if err != nil {
			return Findings{}, err
		}
		defer release()
		h = held.Value(heldKey{c}).(*hold)
	}
	return bounded(ctx, c, h, func() (Findings, error) {
		after, err := c.build(full)
		if err != nil {
			return Findings{}, err
		}
		before := live
		var rebuilt *engine.View
		if stale {
			if before, err = c.build(base); err != nil {
				return Findings{}, err
			}
			rebuilt = before
		}
		afterIssues := after.AllIssues()
		beforeIssues := before.AllIssues()
		found := refusedBy(before, after, beforeIssues, afterIssues, proposed)
		if len(found) > 0 && !stale {
			// The live view was built change by change; a refusal must hold against the same rows built at once.
			if before, err = c.build(base); err != nil {
				return Findings{}, err
			}
			rebuilt = before
			beforeIssues = before.AllIssues()
			found = refusedBy(before, after, beforeIssues, afterIssues, proposed)
		}
		baseline := onceView(rebuilt, func() (*engine.View, error) {
			return bounded(ctx, c, h, func() (*engine.View, error) { return c.build(base) })
		})
		within := func(work func(context.Context) error) error {
			wctx, cancel := context.WithTimeout(ctx, c.timeout)
			defer cancel()
			_, err := bounded(ctx, c, h, func() (struct{}, error) { return struct{}{}, work(wctx) })
			return err
		}
		return Findings{Errors: found, Warnings: newWarnings(beforeIssues, afterIssues), Result: after, baseline: baseline, within: within}, nil
	})
}

// bounded runs build under h's holding of the slot, at most c's bound and while ctx lives. Past the bound, or once ctx ends, it answers ErrCheckTimeout or ctx's error while build runs on, holding the slot, to its end, and a build past the bound makes every check asked for meanwhile answer at once.
func bounded[T any](ctx context.Context, c *Check, h *hold, build func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	h.mu.Lock()
	h.building = true
	h.mu.Unlock()
	go func() {
		defer h.built()
		v, err := build()
		done <- result{v, err}
	}()
	bound := time.NewTimer(c.timeout)
	defer bound.Stop()
	var stopped error
	select {
	case r := <-done:
		return r.v, r.err
	case <-bound.C:
		stopped = ErrCheckTimeout
	case <-ctx.Done():
		stopped = ctx.Err()
	}
	h.mu.Lock()
	if !h.building {
		h.mu.Unlock()
		r := <-done
		return r.v, r.err
	}
	h.abandoned = true
	if stopped == ErrCheckTimeout {
		c.overrun.Store(true)
	}
	h.mu.Unlock()
	var zero T
	return zero, stopped
}

// newWarnings returns the warnings of after that before does not have.
func newWarnings(before, after []engine.Issue) []engine.Issue {
	had := make(map[issueKey]bool, len(before))
	for _, i := range before {
		had[keyOfIssue(i)] = true
	}
	var out []engine.Issue
	for _, i := range after {
		if i.Severity == engine.SeverityWarning && !had[keyOfIssue(i)] {
			out = append(out, i)
		}
	}
	return out
}

// refusedBy is what after refuses that before does not: the errors filed on after alone (their issues given), and a refusal of each lane the proposed rows make or bind (refusedLanes).
func refusedBy(before, after *engine.View, beforeIssues, afterIssues []engine.Issue, proposed []any) []engine.Issue {
	return append(newIssues(beforeIssues, afterIssues), refusedLanes(before, after, proposed)...)
}

// refusedLanes files an error for each lane the proposed rows make (a caller account) or bind (an account of a proposed policy binding) whose caller plan in after refuses every request (CallerPlan.Refused: policy_disabled, duplicate_binding, subject_disabled, account_closed, personal_account_off), unless before refuses it with the same code. No plan files these: a lane refused as a whole is a state, not an error of one row. The issue sits on the binding that binds the lane when one is proposed, else on the lane.
func refusedLanes(before, after *engine.View, proposed []any) []engine.Issue {
	var lanes []string
	bound := map[string]string{}
	for _, p := range proposed {
		switch r := p.(type) {
		case *account.Account:
			if r.ManagedKey() != "" {
				lanes = append(lanes, r.Meta.ID)
			}
		case *policybinding.PolicyBinding:
			for _, id := range r.Spec.AccountIDs {
				if _, ok := bound[id]; !ok {
					lanes = append(lanes, id)
				}
				bound[id] = r.Meta.ID
			}
		}
	}
	var out []engine.Issue
	seen := map[string]bool{}
	for _, id := range lanes {
		if seen[id] {
			continue
		}
		seen[id] = true
		cp := engine.Get[CallerPlan](after, accountSubject(id))
		if cp == nil || cp.Refused == nil {
			continue
		}
		if prev := engine.Get[CallerPlan](before, accountSubject(id)); prev != nil && prev.Refused != nil && prev.Refused.Code == cp.Refused.Code {
			continue
		}
		i := engine.Issue{Severity: engine.SeverityError, Code: cp.Refused.Code, Source: engine.Ref{Kind: KindAccount, ID: id}, Message: "requests in this account would all be refused: " + cp.Refused.Message}
		if b, ok := bound[id]; ok {
			i.Source, i.Field = engine.Ref{Kind: KindPolicyBinding, ID: b}, "spec.policyId"
		}
		if cp.Refused.PolicyID != "" {
			i.Target = engine.Ref{Kind: KindPolicy, ID: cp.Refused.PolicyID}
		}
		out = append(out, i)
	}
	return out
}

// heldKey marks a context whose caller holds c's slot (Hold).
type heldKey struct{ c *Check }

// Hold waits for the check slot, at most the bound and while ctx lives, and returns ctx marked as holding it with the function that frees it; a check made under that ctx runs in the slot without taking it again, and its build gets a bound of its own. A caller that writes what it checks holds the slot across its whole write, so one such write is open at a time and the others wait holding nothing. Past the bound, or at once while a build that outlived its bound still runs, it returns ErrCheckTimeout.
func (c *Check) Hold(ctx context.Context) (context.Context, func(), error) {
	if c.overrun.Load() {
		return ctx, func() {}, ErrCheckTimeout
	}
	wait, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	select {
	case c.slot <- struct{}{}:
	case <-wait.Done():
		return ctx, func() {}, checkStopped(wait)
	}
	h := &hold{c: c}
	return context.WithValue(ctx, heldKey{c}, h), h.release, nil
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

// layer is live's rows of kind T with l's read rows, then its proposed rows, of that kind laid over them; a later row with an id already laid replaces it in place, and a row of an id a proposed Deleted names is left out.
func layer[T any](l *layering, id func(*T) string) rowList[T] {
	out := rowList[T]{id: id}
	at := map[string]int{}
	gone := map[string]bool{}
	for _, x := range l.proposed {
		if d, ok := x.(Deleted); ok {
			if r, ok := d.Row.(*T); ok && r != nil {
				l.used++
				gone[id(r)] = true
			}
		}
	}
	put := func(r *T) {
		if gone[id(r)] {
			return
		}
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
