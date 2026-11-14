package refcheck

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/pkg/meta"
)

// Every lookup a Lookup has is carried by Memoized: a field it dropped would turn its checks off in the lists that use it. A lookup that is off stays off.
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
	m, none := reflect.ValueOf(full.Memoized()), reflect.ValueOf(Lookup{}.Memoized())
	for i := range m.NumField() {
		if m.Field(i).IsNil() {
			t.Errorf("Memoized drops Lookup.%s", m.Type().Field(i).Name)
		}
		if !none.Field(i).IsNil() {
			t.Errorf("Memoized turns on Lookup.%s, which was nil", m.Type().Field(i).Name)
		}
	}
}

// Each memoized lookup reads once per distinct argument and repeats the answer, errors included.
func TestMemoizedReadsOncePerKey(t *testing.T) {
	var reads []string
	boom := errors.New("store down")
	m := Lookup{
		RoleBindingsFor: func(_ context.Context, id string) ([]*rolebinding.RoleBinding, error) {
			reads = append(reads, "bindings-for "+id)
			return []*rolebinding.RoleBinding{{Meta: meta.Metadata{ID: id}}}, boom
		},
		MissingUsers: func(_ context.Context, ids []string) ([]string, error) {
			reads = append(reads, "missing "+ids[0])
			return ids[1:], boom
		},
		UserMayBindIn: func(_ context.Context, userID string, scope meta.Owner) (bool, error) {
			reads = append(reads, "may-bind "+userID+" "+scope.ID)
			return scope == teamOne, boom
		},
		UserVisible: func(_ context.Context, id string) bool {
			reads = append(reads, "visible "+id)
			return id == "u-1"
		},
		UserName: func(_ context.Context, id string) string {
			reads = append(reads, "name "+id)
			return "name of " + id
		},
	}.Memoized()
	ctx := context.Background()
	type answers struct {
		Bindings []string
		Missing  [][]string
		MayBind  []bool
		Visible  []bool
		Names    []string
		Errs     int
	}
	var got answers
	count := func(err error) {
		if errors.Is(err, boom) {
			got.Errs++
		}
	}
	for range 2 {
		for _, id := range []string{"r-1", "r-2"} {
			rbs, err := m.RoleBindingsFor(ctx, id)
			got.Bindings = append(got.Bindings, rbs[0].Meta.ID)
			count(err)
		}
		// ["a","b"] and ["a\x00b"] would share a key if ids were joined without care; ["a"] and ["a","b"] must not.
		for _, ids := range [][]string{{"a"}, {"a", "b"}} {
			missing, err := m.MissingUsers(ctx, ids)
			got.Missing = append(got.Missing, missing)
			count(err)
		}
		for _, scope := range []meta.Owner{teamOne, teamTwo} {
			ok, err := m.UserMayBindIn(ctx, "u-1", scope)
			got.MayBind = append(got.MayBind, ok)
			count(err)
		}
		got.Visible = append(got.Visible, m.UserVisible(ctx, "u-1"), m.UserVisible(ctx, "u-2"))
		got.Names = append(got.Names, m.UserName(ctx, "u-1"))
	}
	want := answers{
		Bindings: []string{"r-1", "r-2", "r-1", "r-2"},
		Missing:  [][]string{{}, {"b"}, {}, {"b"}},
		MayBind:  []bool{true, false, true, false},
		Visible:  []bool{true, false, true, false},
		Names:    []string{"name of u-1", "name of u-1"},
		Errs:     12,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("answers (-want +got):\n%s", diff)
	}
	wantReads := []string{"bindings-for r-1", "bindings-for r-2", "missing a", "missing a", "may-bind u-1 t-1", "may-bind u-1 t-2", "visible u-1", "visible u-2", "name u-1"}
	if diff := cmp.Diff(wantReads, reads); diff != "" {
		t.Errorf("reads (-want +got):\n%s", diff)
	}
}

// A memoized checker answers every row as the plain one does, reading the binding list once for all of them.
func TestMemoizedAnswersAlikeAndReadsOnce(t *testing.T) {
	r, plain := wordRows()
	ctx := as(meID)
	held := []*policybinding.PolicyBinding{{Meta: meta.Metadata{ID: "pb-held", Name: "held"}, Spec: policybinding.Spec{AccountIDs: []string{"acct-p-1"}}}}
	reads := 0
	plain.Rows.PolicyBindings = func(context.Context) ([]*policybinding.PolicyBinding, error) {
		reads++
		return held, nil
	}
	memo := plain
	memo.Rows = plain.Rows.Memoized()
	answers := func(c Checker) map[string]string {
		out := map[string]string{}
		for _, by := range []meta.Owner{system, teamOne, projectOne, {Kind: meta.OwnerUser, ID: meID}} {
			for id, a := range r.accounts {
				out["binding "+id+" by "+by.ID] = refused(c.UsableAccount(ctx, a, by))
				out["pool "+id+" by "+by.ID] = refused(c.UsablePoolAccount(ctx, a, by))
			}
		}
		return out
	}
	want := answers(plain)
	plainReads := reads
	if diff := cmp.Diff(want, answers(memo)); diff != "" {
		t.Errorf("memoized answers (-plain +memoized):\n%s", diff)
	}
	if want["binding acct-p-1 by p-1"] == "" || want["pool acct-team-2 by t-1"] == "" {
		t.Errorf("the rows meant to be refused were accepted: %v", want)
	}
	if plainReads < 2 || reads != plainReads+1 {
		t.Errorf("the binding list was read %d times plain and %d memoized, want several and 1", plainReads, reads-plainReads)
	}
}
