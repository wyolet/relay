// Package engine keeps derived state consistent with a set of input rows.
// Inputs are registered kinds loaded through a Source and changed by Notify,
// Apply and Delete; derived objects are built by controllers, one per derived
// kind, each reconciling one output id at a time from current state. Every
// read a reconcile makes through its Txn is recorded as a dependency edge, so
// the graph that decides what to rebuild is never declared by hand. A drained
// batch of changes runs the controllers to a fixpoint and publishes one
// immutable View; diagnostics (Issue) are part of that derived state.
//
// Out of scope: storage and change feeds (callers implement Source and call
// Notify), the request path (callers read their own structs, stored from the
// OnPublish hook, not the View), metrics types (Hooks report numbers), and the
// contents of runtime cells (package cell). Imports only the standard library
// and package cell.
package engine
