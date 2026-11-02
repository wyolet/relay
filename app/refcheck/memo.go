// Memoized lookups for running one check many times in a request, as a filtered list does: the rows read do not change between rows of one answer, so each is read once. A save runs its check once and uses its Lookup as is.

package refcheck

import (
	"context"
	"strings"
	"sync"

	"github.com/wyolet/relay/app/meta"
)

// Memoized returns l with every read cached for the life of the returned value; nil funcs stay nil. DefaultAccount stays uncached: it may create the account it returns.
func (l Lookup) Memoized() Lookup {
	return Lookup{
		Policy:         memoByID(l.Policy),
		RateLimit:      memoByID(l.RateLimit),
		HostKey:        memoByID(l.HostKey),
		Host:           memoByID(l.Host),
		Account:        memoByID(l.Account),
		Pool:           memoByID(l.Pool),
		Budget:         memoByID(l.Budget),
		Project:        memoByID(l.Project),
		Team:           memoByID(l.Team),
		Role:           memoByID(l.Role),
		ServiceAccount: memoByID(l.ServiceAccount),

		RoleBindingsFor:   memoListByID(l.RoleBindingsFor),
		RoleBindings:      memoList(l.RoleBindings),
		PolicyBindings:    memoList(l.PolicyBindings),
		PolicyBindingsFor: memoListByID(l.PolicyBindingsFor),
		MissingUsers:      memoMissing(l.MissingUsers),
		DefaultAccount:    l.DefaultAccount,
		UserMayBindIn:     memoMayBind(l.UserMayBindIn),
		UserName:          memoName(l.UserName),
	}
}

// cache is a mutex-guarded map: a request's checks run on one goroutine, but the Lookup does not promise that.
type cache[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]V
}

func (c *cache[K, V]) get(k K, read func() V) V {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.m[k]; ok {
		return v
	}
	v := read()
	if c.m == nil {
		c.m = map[K]V{}
	}
	c.m[k] = v
	return v
}

type result[T any] struct {
	v   T
	err error
}

func memoByID[T any](f func(context.Context, string) *T) func(context.Context, string) *T {
	if f == nil {
		return nil
	}
	c := &cache[string, *T]{}
	return func(ctx context.Context, id string) *T {
		return c.get(id, func() *T { return f(ctx, id) })
	}
}

func memoListByID[T any](f func(context.Context, string) ([]*T, error)) func(context.Context, string) ([]*T, error) {
	if f == nil {
		return nil
	}
	c := &cache[string, result[[]*T]]{}
	return func(ctx context.Context, id string) ([]*T, error) {
		r := c.get(id, func() result[[]*T] { v, err := f(ctx, id); return result[[]*T]{v, err} })
		return r.v, r.err
	}
}

func memoList[T any](f func(context.Context) ([]*T, error)) func(context.Context) ([]*T, error) {
	if f == nil {
		return nil
	}
	c := &cache[struct{}, result[[]*T]]{}
	return func(ctx context.Context) ([]*T, error) {
		r := c.get(struct{}{}, func() result[[]*T] { v, err := f(ctx); return result[[]*T]{v, err} })
		return r.v, r.err
	}
}

func memoMissing(f func(context.Context, []string) ([]string, error)) func(context.Context, []string) ([]string, error) {
	if f == nil {
		return nil
	}
	c := &cache[string, result[[]string]]{}
	return func(ctx context.Context, ids []string) ([]string, error) {
		r := c.get(strings.Join(ids, "\x00"), func() result[[]string] { v, err := f(ctx, ids); return result[[]string]{v, err} })
		return r.v, r.err
	}
}

func memoMayBind(f func(context.Context, string, meta.Owner) (bool, error)) func(context.Context, string, meta.Owner) (bool, error) {
	if f == nil {
		return nil
	}
	type k struct {
		user  string
		scope meta.Owner
	}
	c := &cache[k, result[bool]]{}
	return func(ctx context.Context, userID string, scope meta.Owner) (bool, error) {
		r := c.get(k{userID, scope}, func() result[bool] { v, err := f(ctx, userID, scope); return result[bool]{v, err} })
		return r.v, r.err
	}
}

func memoName(f func(context.Context, string) string) func(context.Context, string) string {
	if f == nil {
		return nil
	}
	c := &cache[string, string]{}
	return func(ctx context.Context, id string) string {
		return c.get(id, func() string { return f(ctx, id) })
	}
}
