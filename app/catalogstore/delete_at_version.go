package catalogstore

import (
	"context"
	"errors"

	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/meta"
)

// errNoVersion refuses a conditional delete without a version, which would
// otherwise delete whatever the row has become.
var errNoVersion = errors.New("catalogstore: a conditional delete needs the version that was read")

// deleteAtVersion runs a delete conditional on the row's resource version: a
// row written since, or already gone, is meta.ErrStaleResourceVersion.
func deleteAtVersion(version string, del func(v int64) (int64, error)) error {
	v, ok, err := meta.Metadata{ResourceVersion: version}.ExpectedVersion()
	if err != nil {
		return err
	}
	if !ok {
		return errNoVersion
	}
	return meta.StaleIfNoRows(del(v))
}

// DeleteAtVersion removes the Provider while it still has version.
func (s *ProviderStore) DeleteAtVersion(ctx context.Context, id, version string) error {
	return deleteAtVersion(version, func(v int64) (int64, error) {
		return s.q.DeleteProviderAtVersion(ctx, gen.DeleteProviderAtVersionParams{ID: id, ResourceVersion: v})
	})
}

// DeleteAtVersion removes the Host while it still has version.
func (s *HostStore) DeleteAtVersion(ctx context.Context, id, version string) error {
	return deleteAtVersion(version, func(v int64) (int64, error) {
		return s.q.DeleteHostAtVersion(ctx, gen.DeleteHostAtVersionParams{ID: id, ResourceVersion: v})
	})
}

// DeleteAtVersion removes the Model while it still has version.
func (s *ModelStore) DeleteAtVersion(ctx context.Context, id, version string) error {
	return deleteAtVersion(version, func(v int64) (int64, error) {
		return s.q.DeleteModelAtVersion(ctx, gen.DeleteModelAtVersionParams{ID: id, ResourceVersion: v})
	})
}

// DeleteAtVersion removes the RateLimit while it still has version.
func (s *RateLimitStore) DeleteAtVersion(ctx context.Context, id, version string) error {
	return deleteAtVersion(version, func(v int64) (int64, error) {
		return s.q.DeleteRateLimitAtVersion(ctx, gen.DeleteRateLimitAtVersionParams{ID: id, ResourceVersion: v})
	})
}

// DeleteAtVersion removes the Policy while it still has version.
func (s *PolicyStore) DeleteAtVersion(ctx context.Context, id, version string) error {
	return deleteAtVersion(version, func(v int64) (int64, error) {
		return gen.New(s.db).DeletePolicyAtVersion(ctx, gen.DeletePolicyAtVersionParams{ID: id, ResourceVersion: v})
	})
}

// DeleteAtVersion removes the Pricing while it still has version.
func (s *PricingStore) DeleteAtVersion(ctx context.Context, id, version string) error {
	return deleteAtVersion(version, func(v int64) (int64, error) {
		return gen.New(s.db).DeletePricingAtVersion(ctx, gen.DeletePricingAtVersionParams{ID: id, ResourceVersion: v})
	})
}

// DeleteAtVersion removes the HostBinding while it still has version.
func (s *BindingStore) DeleteAtVersion(ctx context.Context, id, version string) error {
	return deleteAtVersion(version, func(v int64) (int64, error) {
		return gen.New(s.db).DeleteHostBindingAtVersion(ctx, gen.DeleteHostBindingAtVersionParams{ID: id, ResourceVersion: v})
	})
}
