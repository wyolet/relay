package engine

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// settle reconciles dirty outputs, controllers in registration order, until a
// round dirties nothing or MaxRounds is spent.
func (b *batch) settle() {
	e := b.e
	for b.pending() {
		if b.stats.Rounds >= e.opts.MaxRounds {
			b.abandon()
			return
		}
		b.stats.Rounds++
		for _, c := range e.sch.ctls {
			if len(c.dirty) == 0 {
				continue
			}
			ids := make([]string, 0, len(c.dirty))
			for id := range c.dirty {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			clear(c.dirty)
			for _, id := range ids {
				b.reconcile(c, id)
			}
		}
	}
}

func (b *batch) pending() bool {
	for _, c := range b.e.sch.ctls {
		if len(c.dirty) > 0 {
			return true
		}
	}
	return false
}

// abandon gives up on a batch that did not converge (a cycle between
// controllers): what is left dirty keeps its last output and an issue.
func (b *batch) abandon() {
	b.abandoned = true
	for _, c := range b.e.sch.ctls {
		for id := range c.dirty {
			ref := Ref{c.produces, id}
			b.setIssues(issueOwner{by: byAbandonedBatch, out: ref}, []Issue{{
				Severity: SeverityError,
				Code:     "batch_abandoned",
				Source:   ref,
				Message:  fmt.Sprintf("still changing after %d rounds", b.stats.Rounds),
			}})
		}
		clear(c.dirty)
	}
}

func (b *batch) reconcile(c *Controller, id string) {
	e := b.e
	ref := Ref{c.produces, id}
	tx := &Txn{e: e, ctl: c, out: ref}
	err := run(c, tx, id)
	b.stats.Reconciled++
	if err == nil && !tx.done {
		err = errNoOutput
	}
	st := e.st.outs[ref]
	if st == nil {
		st = &outState{}
		e.st.outs[ref] = st
	}
	edges := normalize(tx.edges, cmpEdge)
	if err != nil {
		b.fail(c, ref, st, edges, err)
		return
	}
	delete(e.retries, ref)
	st.failures = 0
	b.setIssues(issueOwner{by: byReconcileFailure, out: ref}, nil)
	b.setIssues(issueOwner{by: byAbandonedBatch, out: ref}, nil)
	b.setEdges(ref, st, edges)
	slices.SortFunc(tx.issues, cmpIssue)
	b.setIssues(issueOwner{ctl: c.name, out: ref}, tx.issues)
	if b.write(c.kind, id, tx.staged) {
		var links = normalize(tx.links, cmpKey)
		if tx.staged == nil {
			links = nil
		}
		b.setLinks(st, links)
	}
	if tx.staged == nil && !c.hasMembers(id) {
		b.forget(c, ref, st)
	}
}

func run(c *Controller, tx *Txn, id string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("engine: controller %s panicked: %v", c.name, r)
		}
	}()
	return c.reconcile(tx, id)
}

// fail keeps the previous output and its links. The failed reconcile's reads
// are added to the previous edges, so fixing what it read retries it at once.
func (b *batch) fail(c *Controller, ref Ref, st *outState, edges []Edge, err error) {
	e := b.e
	b.setEdges(ref, st, normalize(append(slices.Clone(st.edges), edges...), cmpEdge))
	b.setIssues(issueOwner{by: byReconcileFailure, out: ref}, []Issue{{
		Severity: SeverityError,
		Code:     "reconcile_failed",
		Source:   ref,
		Message:  err.Error(),
	}})
	if h := e.opts.Hooks.OnReconcileError; h != nil {
		e.hooks.add(func() { h(c.name, ref, err) })
	}
	delay := e.retryBase()
	if !errors.Is(err, ErrRetry) {
		st.failures++
		for i := 1; i < st.failures && delay < maxBackoff; i++ {
			delay *= 2
		}
		delay = min(delay, maxBackoff)
	}
	e.retries[ref] = time.Now().Add(delay)
}

// forget erases an output that dropped and has no members left: nothing can
// bring it back except a new member, which starts it from scratch.
func (b *batch) forget(c *Controller, ref Ref, st *outState) {
	b.setEdges(ref, st, nil)
	b.setLinks(st, nil)
	b.setIssues(issueOwner{ctl: c.name, out: ref}, nil)
	delete(b.e.st.outs, ref)
	delete(b.e.retries, ref)
}
