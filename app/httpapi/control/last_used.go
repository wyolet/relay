package control

import (
	"context"
	"log/slog"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
)

// enrichKeyRead fills a key read: its principal name and when it last served.
func enrichKeyRead(d Deps) enrichFn[key.Key] {
	names := enrichKey(d)
	return func(ctx context.Context, k *key.Key) {
		if k == nil {
			return
		}
		names(ctx, k)
		d.fillKeysLastUsed(ctx, []*key.Key{k})
	}
}

// enrichKeysRead is enrichKeyRead over a list, with one last-used read for all of it.
func enrichKeysRead(d Deps) enrichListFn[key.Key] {
	names := enrichKeys(d)
	return func(ctx context.Context, keys []*key.Key) {
		names(ctx, keys)
		d.fillKeysLastUsed(ctx, keys)
	}
}

// enrichHostKeyRead fills a host key read: the policies reaching it and when it last served.
func enrichHostKeyRead(d Deps) enrichFn[hostkey.HostKey] {
	policies := enrichHostKeyPolicies(d)
	return func(ctx context.Context, k *hostkey.HostKey) {
		if k == nil {
			return
		}
		policies(ctx, k)
		d.fillHostKeysLastUsed(ctx, []*hostkey.HostKey{k})
	}
}

// enrichHostKeysRead is enrichHostKeyRead over a list, with one last-used read for all of it.
func enrichHostKeysRead(d Deps) enrichListFn[hostkey.HostKey] {
	policies := enrichHostKeyPoliciesAll(d)
	return func(ctx context.Context, keys []*hostkey.HostKey) {
		policies(ctx, keys)
		d.fillHostKeysLastUsed(ctx, keys)
	}
}

// fillKeysLastUsed sets lastUsedAt on keys. A failed read leaves it out rather than failing the response.
func (d Deps) fillKeysLastUsed(ctx context.Context, keys []*key.Key) {
	if d.LastUsed == nil || len(keys) == 0 {
		return
	}
	ids := make([]string, len(keys))
	for i, k := range keys {
		ids[i] = k.Meta.ID
	}
	used, err := d.LastUsed.KeysLastUsed(ctx, ids)
	if err != nil {
		slog.WarnContext(ctx, "keys: last-used read failed", "err", err)
		return
	}
	for _, k := range keys {
		if t, ok := used[k.Meta.ID]; ok {
			k.LastUsedAt = &t
		}
	}
}

// fillHostKeysLastUsed sets lastUsedAt on keys. A failed read leaves it out rather than failing the response.
func (d Deps) fillHostKeysLastUsed(ctx context.Context, keys []*hostkey.HostKey) {
	if d.LastUsed == nil || len(keys) == 0 {
		return
	}
	ids := make([]string, len(keys))
	for i, k := range keys {
		ids[i] = k.Meta.ID
	}
	used, err := d.LastUsed.HostKeysLastUsed(ctx, ids)
	if err != nil {
		slog.WarnContext(ctx, "host-keys: last-used read failed", "err", err)
		return
	}
	for _, k := range keys {
		if t, ok := used[k.Meta.ID]; ok {
			k.LastUsedAt = &t
		}
	}
}
