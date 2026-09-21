package catalog

import (
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/ratelimit"
)

func (s *Snapshot) addRateLimits(rls []*ratelimit.RateLimit, projects idSet) {
	for _, r := range rls {
		clean, keep := sanitizeRateLimit(r, projects)
		if !keep {
			continue
		}
		s.rateLimitsByID[clean.Meta.ID] = clean
		s.rateLimitsByName[clean.Meta.Name] = clean
		s.registerRefs(refKey{Kind: refRateLimit, ID: clean.Meta.ID}, outboundRateLimitRefs(clean))
	}
}

// sanitizeRateLimit drops a project-owned rate limit whose Project is
// missing. Every other owner kind has no parent to check.
func sanitizeRateLimit(r *ratelimit.RateLimit, projects idSet) (*ratelimit.RateLimit, bool) {
	if r.Meta.Owner.Kind == meta.OwnerProject {
		if !projects(r.Meta.Owner.ID) {
			return nil, false
		}
	}
	clean := *r
	return &clean, true
}
