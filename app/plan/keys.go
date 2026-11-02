package plan

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
)

// Root is what the request path loads: the indexes of one published generation.
type Root struct {
	Keys  *KeyPlanIndex
	Users *UserIndex
	Lanes *LaneIndex
	// Catalog resolves requests that name no caller (ResolveAtHost).
	Catalog *CatalogIndex
	Gen     uint64 // the generation it was published as, for caches keyed by it
}

// Holder keeps the current Root behind one atomic pointer, swapped when the engine publishes.
type Holder struct{ root atomic.Pointer[Root] }

// Load returns the current Root, nil before the first publish.
func (h *Holder) Load() *Root { return h.root.Load() }

// OnPublish is the engine's OnPublish hook: it stores the Root of the view just published.
func (h *Holder) OnPublish(v *engine.View, _ engine.PublishStats) {
	h.root.Store(&Root{
		Keys:    engine.Get[KeyPlanIndex](v, ""),
		Users:   engine.Get[UserIndex](v, ""),
		Lanes:   engine.Get[LaneIndex](v, ""),
		Catalog: engine.Get[CatalogIndex](v, ""),
		Gen:     uint64(v.Gen()),
	})
}

// KeyPlan is what authentication derives for one key: its principal and its default lane.
type KeyPlan struct {
	Key       *key.Key
	Principal key.Principal
	// Caller is the key's default lane; a refusing plan when the principal cannot use it.
	Caller *CallerPlan
	// GraceEnds is when the pre-rotation hash stops authenticating; zero without a rotation.
	GraceEnds time.Time
	in        inputs
}

func (k *KeyPlan) builtFrom() inputs { return k.in }

// KeyPlanShard holds the key plans whose hash (or in-grace previous hash) starts with one byte.
type KeyPlanShard struct {
	Index  int
	ByHash map[string]*KeyPlan
}

// KeyPlanIndex finds a key plan by hash. 256 shards on the hash's first byte, so a key write copies one shard's map.
type KeyPlanIndex struct{ Shards [256]*KeyPlanShard }

// Lookup returns the plan of the key whose hash, or previous hash still in grace, is h, if that key is active at now; nil otherwise.
func (x *KeyPlanIndex) Lookup(h string, now time.Time) *KeyPlan {
	if x == nil {
		return nil
	}
	i, ok := shardOf(h)
	if !ok {
		return nil
	}
	sh := x.Shards[i]
	if sh == nil {
		return nil
	}
	kp := sh.ByHash[h]
	if kp == nil {
		return nil
	}
	if h != kp.Key.Spec.KeyHash && !now.Before(kp.GraceEnds) {
		return nil
	}
	if exp := kp.Key.Spec.ExpiresAt; exp != nil && !now.Before(*exp) {
		return nil
	}
	return kp
}

// shardOf reads a hex hash's first byte.
func shardOf(h string) (int, bool) {
	if len(h) < 2 {
		return 0, false
	}
	hi, ok1 := hexVal(h[0])
	lo, ok2 := hexVal(h[1])
	return hi<<4 | lo, ok1 && ok2
}

func hexVal(c byte) (int, bool) {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0'), true
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10, true
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// shardID is the id of the shard holding hash h, lowercased so both hex cases land in one shard; "" for a hash too short.
func shardID(h string) string {
	if len(h) < 2 {
		return ""
	}
	return strings.ToLower(h[:2])
}

// keyShards is a key plan's Watch key: the shard of its hash and, during a rotation's grace, of its previous hash.
func keyShards(kp *KeyPlan) []string {
	out := make([]string, 0, 2)
	cur := shardID(kp.Key.Spec.KeyHash)
	if cur != "" {
		out = append(out, cur)
	}
	if prev := shardID(kp.Key.Spec.PreviousKeyHash); prev != "" && prev != cur && !kp.GraceEnds.IsZero() {
		out = append(out, prev)
	}
	return out
}

func (c *controllers) keyPlan(tx *engine.Txn, id string) error {
	k := engine.Get[key.Key](tx, id)
	if k == nil || !k.IsEnabled() || k.Spec.RevokedAt != nil {
		engine.Drop(tx)
		return nil
	}
	src := engine.Ref{Kind: KindKey, ID: id}
	laneID := k.Spec.AccountID
	if laneID == "" {
		// A key saved without a lane runs in its principal's own account.
		if m := engine.Get[ManagedAccount](tx, ownAccountKey(k.Spec.Principal)); m != nil && m.Row != nil {
			laneID = m.Row.Meta.ID
		}
	}
	var cp *CallerPlan
	if laneID != "" {
		cp = engine.Get[CallerPlan](tx, accountSubject(laneID))
	}
	if cp == nil {
		tx.Issue(parentMissing(src, "spec.accountId", engine.Ref{Kind: KindAccount, ID: laneID}))
		engine.Drop(tx)
		return nil
	}
	kp := &KeyPlan{Key: k, Principal: k.Spec.Principal, Caller: cp, in: inputs{k, cp}}
	principal := principalKey(k.Spec.Principal)
	switch {
	case !cp.Lane.IsProvider() && cp.Holder != principal:
		kp.Caller = refusedPlan(cp.Lane, &Refusal{Code: CodeAccountNotHeld, Rung: limits.RungLane, SubjectID: accountSubject(cp.Lane.Meta.ID), Message: "account " + cp.Lane.Meta.Name + " is not held by " + principal})
	case cp.Lane.IsProvider():
		// A provider account has no holder whose state the lane plan reads, so the key's own principal is checked here.
		if disabled, ok := principalDisabled(tx, k.Spec.Principal); !ok {
			tx.Issue(parentMissing(src, "spec.principal", engine.Ref{Kind: engine.Kind(k.Spec.Principal.Kind), ID: k.Spec.Principal.ID}))
			engine.Drop(tx)
			return nil
		} else if disabled {
			kp.in = append(kp.in, disabled)
			kp.Caller = refusedPlan(cp.Lane, &Refusal{Code: CodeSubjectDisabled, Rung: limits.RungLane, SubjectID: principal, Message: principal + " is disabled"})
		}
	}
	if k.Spec.PreviousKeyHash != "" && k.Spec.GraceUntil != nil {
		kp.GraceEnds = *k.Spec.GraceUntil
	}
	engine.Put(tx, SameOr(engine.Get[KeyPlan](tx, id), kp))
	return nil
}

// principalKey renders a key's principal as "user:<id>" or "serviceaccount:<id>".
func principalKey(p key.Principal) string { return string(p.Kind) + ":" + p.ID }

// ownAccountKey is the place of a principal's own account: a user's personal account, a service account's account.
func ownAccountKey(p key.Principal) string {
	if p.Kind == key.PrincipalServiceAccount {
		return "service:" + p.ID
	}
	return "personal:" + p.ID
}

// principalDisabled reads whether the principal is switched off; ok is false when its row is gone.
func principalDisabled(tx *engine.Txn, p key.Principal) (disabled, ok bool) {
	switch p.Kind {
	case key.PrincipalUser:
		if u := engine.Get[user.User](tx, p.ID); u != nil {
			return u.Disabled, true
		}
	case key.PrincipalServiceAccount:
		if sa := engine.Get[serviceaccount.ServiceAccount](tx, p.ID); sa != nil {
			return !sa.IsEnabled(), true
		}
	}
	return false, false
}

// LaneIndex finds a lane's plan by account id or name, and a user's member account in a team or project.
type LaneIndex struct {
	byID, byName map[string]*CallerPlan
	// member is user id → team or project scope → the user's member lane there; nested so a lookup builds no key.
	member map[string]map[meta.Owner]*CallerPlan
}

// Lookup returns the plan of the lane named by ref, an account id or name; nil when there is none.
func (x *LaneIndex) Lookup(ref string) *CallerPlan {
	if x == nil {
		return nil
	}
	if cp := x.byID[ref]; cp != nil {
		return cp
	}
	return x.byName[ref]
}

// Member returns the plan of userID's member account in scope, nil when the user has none there.
func (x *LaneIndex) Member(userID string, scope meta.Owner) *CallerPlan {
	if x == nil {
		return nil
	}
	return x.member[userID][scope]
}

func (c *controllers) laneIndex(tx *engine.Txn, _ string) error {
	cps := engine.Members[CallerPlan](tx)
	x := &LaneIndex{byID: make(map[string]*CallerPlan, len(cps)), byName: make(map[string]*CallerPlan, len(cps)), member: map[string]map[meta.Owner]*CallerPlan{}}
	for _, cp := range cps {
		x.byID[cp.Lane.Meta.ID], x.byName[cp.Lane.Meta.Name] = cp, cp
		if cp.Lane.DerivedKind() != account.KindMember {
			continue
		}
		user := cp.Lane.Spec.Holder.ID
		if x.member[user] == nil {
			x.member[user] = map[meta.Owner]*CallerPlan{}
		}
		x.member[user][cp.Lane.Meta.Owner] = cp
	}
	engine.Put(tx, x)
	return nil
}

// HeldBy reports whether principal ("user:<id>" or "serviceaccount:<id>") may run requests in this lane: it holds the account, or owns the provider account.
func (p *CallerPlan) HeldBy(principal string) bool {
	if p.Lane.IsProvider() {
		return ownerRef(p.Lane.Meta.Owner) == principal
	}
	return p.Holder == principal
}

// LaneFor resolves the lane a key's request runs in: ref, an account id or name the key's principal holds, or the key's default lane when ref is empty or names it. A lane the principal does not hold refuses with account_not_held, the same answer for an account that does not exist.
func (r *Root) LaneFor(kp *KeyPlan, ref string) (*CallerPlan, *Refusal) {
	if ref == "" || ref == kp.Caller.Lane.Meta.ID || ref == kp.Caller.Lane.Meta.Name {
		return kp.Caller, nil
	}
	return r.Lane(principalKey(kp.Principal), ref)
}

// Lane resolves the lane named by ref, an account id or name, for principal; account_not_held when the principal does not hold it or it does not exist.
func (r *Root) Lane(principal, ref string) (*CallerPlan, *Refusal) {
	cp := r.Lanes.Lookup(ref)
	if cp == nil || !cp.HeldBy(principal) {
		return nil, &Refusal{Code: CodeAccountNotHeld, Message: "no account " + ref + " held by " + principal}
	}
	return cp, nil
}

// MemberLane is the lane a user's request inside a project runs in, as a relay-minted token scoped to the project names: the user's member account there.
func (r *Root) MemberLane(userID, projectID string) (*CallerPlan, *Refusal) {
	if cp := r.Lanes.Member(userID, meta.Owner{Kind: meta.OwnerProject, ID: projectID}); cp != nil {
		return cp, nil
	}
	return nil, &Refusal{Code: CodeAccountNotHeld, Message: "user:" + userID + " holds no account in project " + projectID}
}

func (c *controllers) keyPlanShard(tx *engine.Txn, id string) error {
	i, ok := shardOf(id)
	kps := engine.Members[KeyPlan](tx)
	if !ok || len(kps) == 0 {
		engine.Drop(tx)
		return nil
	}
	sh := &KeyPlanShard{Index: i, ByHash: make(map[string]*KeyPlan, len(kps))}
	// Current hashes first: a hash in another key's grace never takes a live key's slot.
	for _, kp := range kps {
		if h := kp.Key.Spec.KeyHash; shardID(h) == id {
			if _, taken := sh.ByHash[h]; !taken {
				sh.ByHash[h] = kp
			}
		}
	}
	for _, kp := range kps {
		if p := kp.Key.Spec.PreviousKeyHash; !kp.GraceEnds.IsZero() && shardID(p) == id {
			if _, taken := sh.ByHash[p]; !taken {
				sh.ByHash[p] = kp
			}
		}
	}
	engine.Put(tx, sh)
	return nil
}

func (c *controllers) keyPlanIndex(tx *engine.Txn, _ string) error {
	idx := &KeyPlanIndex{}
	for _, sh := range engine.Members[KeyPlanShard](tx) {
		idx.Shards[sh.Index] = sh
	}
	engine.Put(tx, idx)
	return nil
}
