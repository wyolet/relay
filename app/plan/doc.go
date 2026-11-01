// Package plan folds the policy ladder into the structs the request path reads, and registers the engine controllers that keep them current. The ladder runs Host → Account → Credential on the upstream side and Team → Project → Principal on the caller side; each rung holds at most one policy, every rung only narrows, and a key holds none: it points at its principal's CallerPlan.
//
// Write side: Register declares the input kinds with their sources and one controller per derived kind (catalog index, expanded policies, rung policies, account, credential and pool plans, owned credentials, caller plans, key plans and their index). Read side: Holder.Load returns the Root; a request goes key hash → KeyPlan → CallerPlan → RouteTable.Get → Route → hops and slots, and hands a slot's counters to a limits.Limiter. Nothing on the read side calls into the engine.
//
// Out of scope: counting (app/limits), picking a credential inside a hop and the breaker and cooldown transitions (app/keypool), the request pipeline, relay-minted tokens, model groups, and the save-time check that a policy links only what its author may use.
package plan
