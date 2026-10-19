package keyring_test

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/secret/kek/local"
	"github.com/wyolet/relay/secret/keyring"
)

// KEYRING_PROPERTY_SEEDS and KEYRING_PROPERTY_STEPS lengthen the run, e.g. 5000 and 400 for a long soak.
func propertySize(t *testing.T) (seeds, steps int) {
	return envInt(t, "KEYRING_PROPERTY_SEEDS", 40), envInt(t, "KEYRING_PROPERTY_STEPS", 150)
}

func envInt(t *testing.T, name string, def int) int {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		t.Fatalf("%s=%q: want a positive integer", name, s)
	}
	return n
}

// world is two Rings sharing one store and one row store. Key changes made by one ring reach the other only when a simulated notification is delivered, and deliveries are randomly dropped.
type world struct {
	t     *testing.T
	rnd   *rand.Rand
	clock *clock
	rows  *rows
	rings [2]*keyring.Ring
	keks  [2]*local.KEK
	want  map[string][]byte // row name → plaintext
	log   []string

	refusedWrites, destroyed, rewraps, missed int
}

var propertyScopes = []string{"", "tenant-a", "tenant-b"}

func rowScope(name string) string { return propertyScopes[int(name[1]-'0')%len(propertyScopes)] }

func TestPropertyTwoRings(t *testing.T) {
	seeds, steps := propertySize(t)
	start := time.Now()
	var refused, destroyed, rewraps, missed int
	for seed := 0; seed < seeds; seed++ {
		w := newWorld(t, uint64(seed))
		for step := 0; step < steps; step++ {
			w.step()
		}
		w.final()
		refused += w.refusedWrites
		destroyed += w.destroyed
		rewraps += w.rewraps
		missed += w.missed
	}
	// The interesting paths must actually be exercised.
	if seeds >= 20 && (refused == 0 || destroyed == 0 || rewraps == 0 || missed == 0) {
		t.Fatalf("coverage too thin: refused writes %d, destroys %d, rewraps %d, missed notifications %d", refused, destroyed, rewraps, missed)
	}
	t.Logf("%d seeds × %d steps in %s: refused writes %d, destroys %d, rewraps %d, missed notifications %d", seeds, steps, time.Since(start).Round(time.Millisecond), refused, destroyed, rewraps, missed)
}

func newWorld(t *testing.T, seed uint64) *world {
	w := &world{
		t:     t,
		rnd:   rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		clock: newClock(),
		rows:  newRows(),
		keks:  [2]*local.KEK{newKEK(t, 1), newKEK(t, 2)},
		want:  map[string][]byte{},
	}
	w.log = append(w.log, fmt.Sprint("seed ", seed))
	for i := range w.rings {
		r, err := keyring.New(ctx, w.rows.ms, newSet(t, w.keks[0], w.keks[1]), keyring.Options{Now: w.clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		w.rings[i] = r
	}
	return w
}

func (w *world) fail(format string, args ...any) {
	w.t.Helper()
	w.t.Fatalf("%s\nops:\n%s", fmt.Sprintf(format, args...), strings.Join(w.log, "\n"))
}

func (w *world) step() {
	w.t.Helper()
	x := w.rnd.IntN(2)
	r := w.rings[x]
	before := w.storeState()
	var op string
	switch n := w.rnd.IntN(100); {
	case n < 30:
		name := fmt.Sprint("r", w.rnd.IntN(6))
		op = fmt.Sprintf("ring%d write %s", x, name)
		w.write(r, name, fmt.Appendf(nil, "%s-%d", name, w.rnd.Uint32()))
	case n < 42:
		scope := propertyScopes[w.rnd.IntN(len(propertyScopes))]
		op = fmt.Sprintf("ring%d rotate %q", x, scope)
		if _, err := r.Rotate(ctx, scope); err != nil {
			w.fail("%s: %v", op, err)
		}
	case n < 48:
		to := w.keks[w.rnd.IntN(2)].ID()
		op = fmt.Sprintf("ring%d rewrap to %x", x, to[:2])
		if _, err := r.Rewrap(ctx, to); err != nil {
			w.fail("%s: %v", op, err)
		}
		w.rewraps++
	case n < 60:
		all, _ := w.rows.ms.List(ctx)
		if len(all) == 0 {
			return
		}
		k := all[w.rnd.IntN(len(all))]
		op = fmt.Sprintf("ring%d destroy %s (%s)", x, keyring.FormatID(k.ID), k.State)
		err := r.Destroy(ctx, k.ID)
		switch {
		case err == nil:
			if k.State != keyring.Destroyed {
				w.destroyed++
			}
			if w.rows.inUse(k.ID) {
				w.fail("%s: destroyed a key in use", op)
			}
		case errors.Is(err, keyring.ErrIsPrimary), errors.Is(err, keyring.ErrInUse), errors.Is(err, keyring.ErrTooRecent):
		default:
			w.fail("%s: %v", op, err)
		}
	case n < 70:
		op = fmt.Sprintf("ring%d rewrite stale rows", x)
		w.rewriteStale(r)
	case n < 80:
		d := time.Duration(w.rnd.IntN(30)) * time.Hour
		op = fmt.Sprint("advance ", d)
		w.clock.Advance(d)
	case n < 88:
		op = fmt.Sprintf("ring%d reload all", x)
		if err := r.ReloadAll(ctx); err != nil {
			w.fail("%s: %v", op, err)
		}
	default:
		op = "check"
		w.checkDecrypts(false)
	}
	w.log = append(w.log, op)
	w.notify(1-x, before)
}

// notify delivers the key changes since before to the other ring, unless the notification is lost.
func (w *world) notify(to int, before map[[16]byte]string) {
	w.t.Helper()
	after := w.storeState()
	for id, s := range after {
		if before[id] == s {
			continue
		}
		if w.rnd.IntN(10) < 3 {
			w.missed++
			w.log = append(w.log, fmt.Sprintf("  ring%d misses %s", to, keyIDOrTarget(id)))
			continue
		}
		var err error
		if id == targetSlot {
			err = w.rings[to].ReloadTarget(ctx)
		} else {
			err = w.rings[to].Reload(ctx, id)
		}
		if err != nil {
			w.fail("notify: %v", err)
		}
	}
}

var targetSlot = [16]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

func keyIDOrTarget(id [16]byte) string {
	if id == targetSlot {
		return "target"
	}
	return keyring.FormatID(id)
}

func (w *world) storeState() map[[16]byte]string {
	all, err := w.rows.ms.List(ctx)
	if err != nil {
		w.fail("List: %v", err)
	}
	out := make(map[[16]byte]string, len(all)+1)
	for _, k := range all {
		out[k.ID] = fmt.Sprintf("%s/%x", k.State, k.KEKID)
	}
	t, _ := w.rows.ms.TargetKEK(ctx)
	out[targetSlot] = fmt.Sprintf("%x", t)
	return out
}

func (w *world) write(r *keyring.Ring, name string, plain []byte) {
	w.t.Helper()
	scope := rowScope(name)
	refusedBefore := w.rows.refused.Load()
	if err := r.EncryptAndWrite(ctx, scope, plain, []byte(name), w.rows.writer(name, scope)); err != nil {
		w.fail("write %s: %v", name, err)
	}
	w.refusedWrites += int(w.rows.refused.Load() - refusedBefore)
	w.want[name] = plain
	// Nothing ran since the write, so the row's key must be its scope's Primary.
	p, err := w.rows.ms.Primary(ctx, scope)
	if err != nil || p.ID != w.rows.get(name).keyID {
		w.fail("write %s: row under %s, scope Primary %s (%v)", name, keyring.FormatID(w.rows.get(name).keyID), keyring.FormatID(p.ID), err)
	}
}

func (w *world) rewriteStale(r *keyring.Ring) {
	w.t.Helper()
	for name, rw := range w.rows.snapshot() {
		plain, st, err := r.Decrypt(ctx, rw.scope, rw.sealed, []byte(name))
		if err != nil {
			w.fail("rewrite: decrypt %s: %v", name, err)
		}
		if st.Stale {
			w.write(r, name, plain)
		}
	}
}

// checkDecrypts asserts every live row decrypts on both rings; with exactStatus it also asserts Stale matches the store.
func (w *world) checkDecrypts(exactStatus bool) {
	w.t.Helper()
	for name, rw := range w.rows.snapshot() {
		k, err := w.rows.ms.Get(ctx, rw.keyID)
		if err != nil || k.State == keyring.Destroyed {
			w.fail("row %s under %s: %+v, %v", name, keyring.FormatID(rw.keyID), k.State, err)
		}
		primary, _ := w.rows.ms.Primary(ctx, rw.scope)
		for i, r := range w.rings {
			plain, st, err := r.Decrypt(ctx, rw.scope, rw.sealed, []byte(name))
			if err != nil {
				w.fail("ring%d: decrypt %s: %v", i, name, err)
			}
			if !bytes.Equal(plain, w.want[name]) {
				w.fail("ring%d: decrypt %s = %q, want %q", i, name, plain, w.want[name])
			}
			if exactStatus && st.Stale != (primary.ID != rw.keyID) {
				w.fail("ring%d: %s stale = %v, key %s, scope Primary %s", i, name, st.Stale, keyring.FormatID(rw.keyID), keyring.FormatID(primary.ID))
			}
		}
	}
}

func (w *world) final() {
	w.t.Helper()
	w.checkDecrypts(false)
	for i, r := range w.rings {
		if err := r.ReloadAll(ctx); err != nil {
			w.fail("ring%d reload: %v", i, err)
		}
	}
	w.checkDecrypts(true)
	for i, r := range w.rings {
		for _, k := range r.Keys() {
			if k.Locked {
				w.fail("ring%d: key %s locked", i, keyring.FormatID(k.ID))
			}
		}
	}
}
