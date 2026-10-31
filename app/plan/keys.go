package plan

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/engine"
)

// Root is what the request path loads: the key index of one published generation.
type Root struct {
	Keys *KeyPlanIndex
	Gen  uint64 // the generation it was published as, for caches keyed by it
}

// Holder keeps the current Root behind one atomic pointer, swapped when the engine publishes.
type Holder struct{ root atomic.Pointer[Root] }

// Load returns the current Root, nil before the first publish.
func (h *Holder) Load() *Root { return h.root.Load() }

// OnPublish is the engine's OnPublish hook: it stores the Root of the view just published.
func (h *Holder) OnPublish(v *engine.View, _ engine.PublishStats) {
	h.root.Store(&Root{Keys: engine.Get[KeyPlanIndex](v, ""), Gen: uint64(v.Gen())})
}

// KeyPlan is what authentication derives for one key; the ladder is its principal's.
type KeyPlan struct {
	Key       *key.Key
	Principal key.Principal
	Caller    *CallerPlan
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
	principal := string(k.Spec.Principal.Kind) + ":" + k.Spec.Principal.ID
	cp := engine.Get[CallerPlan](tx, principal)
	if cp == nil {
		tx.Issue(parentMissing(engine.Ref{Kind: KindKey, ID: id}, "spec.principal", engine.Ref{Kind: engine.Kind(k.Spec.Principal.Kind), ID: k.Spec.Principal.ID}))
		engine.Drop(tx)
		return nil
	}
	kp := &KeyPlan{Key: k, Principal: k.Spec.Principal, Caller: cp, in: inputs{k, cp}}
	if k.Spec.PreviousKeyHash != "" && k.Spec.GraceUntil != nil {
		kp.GraceEnds = *k.Spec.GraceUntil
	}
	engine.Put(tx, SameOr(engine.Get[KeyPlan](tx, id), kp))
	return nil
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
