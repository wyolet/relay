package seed

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/ratelimit"
)

// systemRateLimitStore is the part of ratelimit.Store the system seed uses.
type systemRateLimitStore interface {
	List(ctx context.Context) ([]*ratelimit.RateLimit, error)
	Upsert(ctx context.Context, rl *ratelimit.RateLimit) error
}

// SystemRateLimits is the set of system rate limit definitions a boot seeds.
type SystemRateLimits struct {
	Rows []*ratelimit.RateLimit
	// FromDir names the definitions taken from the directory instead of the shipped ones.
	FromDir []string
	// Ignored lists the directory documents that are not a system rate limit the binary ships, as "Kind/name".
	Ignored []string
}

// LoadSystemRateLimits parses the definitions shipped in the binary, then replaces each one a YAML file under dir also defines. Only shipped names are taken from dir: it changes what a built-in row starts as, it does not add rows. A missing dir is not an error.
func LoadSystemRateLimits(shipped []byte, dir string) (*SystemRateLimits, error) {
	docs, err := manifest.Parse(bytes.NewReader(shipped))
	if err != nil {
		return nil, fmt.Errorf("system rate limits: %w", err)
	}
	out := &SystemRateLimits{}
	position := map[string]int{}
	for _, d := range docs {
		rl, err := systemRateLimit(d)
		if err != nil {
			return nil, fmt.Errorf("system rate limits: %w", err)
		}
		if _, dup := position[rl.Meta.Name]; dup {
			return nil, fmt.Errorf("system rate limits: %q is defined twice", rl.Meta.Name)
		}
		position[rl.Meta.Name] = len(out.Rows)
		out.Rows = append(out.Rows, rl)
	}

	if _, err := os.Stat(dir); dir == "" || os.IsNotExist(err) {
		return out, nil
	}
	dirDocs, err := manifest.LoadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("system rate limits: %w", err)
	}
	replaced := map[string]bool{}
	for _, d := range dirDocs {
		i, shippedName := position[docName(d)]
		if d.RateLimit == nil || d.RateLimit.Metadata.Owner.Kind != meta.OwnerSystem || !shippedName {
			out.Ignored = append(out.Ignored, d.Kind()+"/"+docName(d))
			continue
		}
		rl, err := systemRateLimit(d)
		if err != nil {
			return nil, fmt.Errorf("system rate limits: %s: %w", dir, err)
		}
		if replaced[rl.Meta.Name] {
			return nil, fmt.Errorf("system rate limits: %s: %q is defined twice", dir, rl.Meta.Name)
		}
		replaced[rl.Meta.Name] = true
		out.Rows[i] = rl
		out.FromDir = append(out.FromDir, rl.Meta.Name)
	}
	sort.Strings(out.FromDir)
	return out, nil
}

// systemRateLimit translates one document into a system-owned row with no id.
func systemRateLimit(d manifest.Document) (*ratelimit.RateLimit, error) {
	if d.RateLimit == nil {
		return nil, fmt.Errorf("%s %q: only RateLimit documents are accepted", d.Kind(), docName(d))
	}
	rl, err := manifest.ToRateLimit(*d.RateLimit, manifest.MapResolver{})
	if err != nil {
		return nil, err
	}
	if rl.Meta.Owner.Kind != meta.OwnerSystem {
		return nil, fmt.Errorf("ratelimit %q: owner.kind must be system", rl.Meta.Name)
	}
	rl.Meta.ID = ""
	if err := rl.Validate(); err != nil {
		return nil, err
	}
	return rl, nil
}

// CreateMissingSystemRateLimits writes each definition whose name has no row and returns the names it created. A row that exists is never written, whoever owns it and whatever it holds, so an operator's edit or disable survives every boot.
func CreateMissingSystemRateLimits(ctx context.Context, store systemRateLimitStore, defs []*ratelimit.RateLimit) ([]string, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	stored, err := storedRateLimitNames(ctx, store)
	if err != nil {
		return nil, err
	}
	var created []string
	for _, def := range defs {
		if stored[def.Meta.Name] {
			continue
		}
		row := *def
		row.Meta.ID = meta.NewID()
		if err := store.Upsert(ctx, &row); err != nil {
			// Names are unique, so a writer that created the same row since the list makes this insert fail. That row is the one to keep.
			now, listErr := storedRateLimitNames(ctx, store)
			if listErr != nil || !now[def.Meta.Name] {
				return created, fmt.Errorf("system rate limits: create %q: %w", def.Meta.Name, err)
			}
			continue
		}
		created = append(created, def.Meta.Name)
	}
	return created, nil
}

func storedRateLimitNames(ctx context.Context, store systemRateLimitStore) (map[string]bool, error) {
	rows, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("system rate limits: %w", err)
	}
	names := make(map[string]bool, len(rows))
	for _, r := range rows {
		names[r.Meta.Name] = true
	}
	return names, nil
}
