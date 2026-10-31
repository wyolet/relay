// Runtime state the plans link as cells (pkg/engine/cell): one breaker copy per credential value, one cooldown per provider account, one rotation per pool. The request path reads them through atomics only; watchers and the picker write them.

package keypool

import (
	"sync"
	"sync/atomic"

	"github.com/wyolet/relay/pkg/engine/cell"
)

// Cell types, so two kinds of cell never share an id.
const (
	cellBreaker  = "breaker"
	cellCooldown = "account-cooldown"
	cellRotation = "pool-rotation"
)

// BreakerCellKey keys a breaker by the credential's value hash: a rotated value gets a fresh, closed breaker and the old one expires unlinked.
func BreakerCellKey(valueHash string) cell.Key { return cell.Key{Type: cellBreaker, ID: valueHash} }

// BreakerCell holds the latest circuit record known for one credential value.
type BreakerCell struct{ rec atomic.Pointer[CircuitRecord] }

// NewBreakerCell returns a closed breaker.
func NewBreakerCell() *BreakerCell { return &BreakerCell{} }

// Load returns the record, nil while the breaker has never failed.
func (c *BreakerCell) Load() *CircuitRecord { return c.rec.Load() }

// Store replaces the record; r must not be modified afterwards.
func (c *BreakerCell) Store(r *CircuitRecord) { c.rec.Store(r) }

// CooldownCellKey keys a cooldown by account id: every credential of the account shares the provider's ceiling, so they cool together.
func CooldownCellKey(accountID string) cell.Key { return cell.Key{Type: cellCooldown, ID: accountID} }

// CooldownCell is the time, in unix milliseconds, until which an account is skipped.
type CooldownCell struct{ until atomic.Int64 }

// NewCooldownCell returns a cooldown that is not set.
func NewCooldownCell() *CooldownCell { return &CooldownCell{} }

// Until returns the cooldown end in unix milliseconds, 0 when never set.
func (c *CooldownCell) Until() int64 { return c.until.Load() }

// Cooled reports whether the account is cooling at nowMs.
func (c *CooldownCell) Cooled(nowMs int64) bool { return nowMs < c.until.Load() }

// Extend moves the end to untilMs unless it already lies later, so a short local cooldown never cuts a longer one another pod reported.
func (c *CooldownCell) Extend(untilMs int64) {
	for {
		cur := c.until.Load()
		if untilMs <= cur || c.until.CompareAndSwap(cur, untilMs) {
			return
		}
	}
}

// RotationKey keys a pool's rotation by pool id, so reordering the pool keeps its round-robin phase and every policy drawing on the pool shares one rotation.
func RotationKey(poolID string) cell.Key { return cell.Key{Type: cellRotation, ID: poolID} }

// Rotation is one pool's per-process selection state: the round-robin counter and the last use of each credential value.
type Rotation struct {
	next     atomic.Uint64
	lastUsed sync.Map // value hash → unix ms
}

// NewRotation returns a rotation starting at the pool's first credential.
func NewRotation() *Rotation { return &Rotation{} }

// Next returns the round-robin counter and advances it.
func (r *Rotation) Next() uint64 { return r.next.Add(1) - 1 }

// Touch records that the credential with valueHash served at atMs.
func (r *Rotation) Touch(valueHash string, atMs int64) { r.lastUsed.Store(valueHash, atMs) }

// LastUsed returns when the credential last served, 0 if never.
func (r *Rotation) LastUsed(valueHash string) int64 {
	v, ok := r.lastUsed.Load(valueHash)
	if !ok {
		return 0
	}
	return v.(int64)
}
