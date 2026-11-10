package refcheck

import (
	"context"
	"reflect"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/meta"
)

// Every lookup a Lookup has is carried by Memoized: a field it dropped would turn its checks off in the lists that use it.
func TestMemoizedKeepsEveryLookup(t *testing.T) {
	var full Lookup
	v := reflect.ValueOf(&full).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		f.Set(reflect.MakeFunc(f.Type(), func([]reflect.Value) []reflect.Value {
			out := make([]reflect.Value, f.Type().NumOut())
			for j := range out {
				out[j] = reflect.Zero(f.Type().Out(j))
			}
			return out
		}))
	}
	m := reflect.ValueOf(full.Memoized())
	for i := range m.NumField() {
		if m.Field(i).IsNil() {
			t.Errorf("Memoized drops Lookup.%s", m.Type().Field(i).Name)
		}
	}
}

// A memoized checker answers every row as the plain one does, reading the binding list once for all of them.
func TestMemoizedAnswersAlikeAndReadsOnce(t *testing.T) {
	r, plain := wordRows()
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: meID})
	held := []*policybinding.PolicyBinding{{Meta: meta.Metadata{ID: "pb-held", Name: "held"}, Spec: policybinding.Spec{AccountIDs: []string{"acct-p-1"}}}}
	reads := 0
	plain.Rows.PolicyBindings = func(context.Context) ([]*policybinding.PolicyBinding, error) {
		reads++
		return held, nil
	}
	memo := plain
	memo.Rows = plain.Rows.Memoized()
	for _, by := range []meta.Owner{{Kind: meta.OwnerSystem}, {Kind: meta.OwnerTeam, ID: "t-1"}, {Kind: meta.OwnerProject, ID: "p-1"}, {Kind: meta.OwnerUser, ID: meID}} {
		for id, a := range r.accounts {
			for name, check := range map[string]func(Checker) error{
				"binding": func(c Checker) error { return c.UsableAccount(ctx, a, by) },
				"pool":    func(c Checker) error { return c.UsablePoolAccount(ctx, a, by) },
			} {
				want, got := check(plain), check(memo)
				if status(want) != status(got) || code(want) != code(got) {
					t.Errorf("%s %s by %v: memoized %v, plain %v", name, id, by, got, want)
				}
			}
		}
	}
	before := reads
	for _, a := range r.accounts {
		_ = memo.UsableAccount(ctx, a, meta.Owner{Kind: meta.OwnerProject, ID: "p-1"})
	}
	if reads != before {
		t.Errorf("the memoized checker read the bindings %d more times, want 0 after the first", reads-before)
	}
}
