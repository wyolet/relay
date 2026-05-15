package catalog

import "github.com/wyolet/relay/app/ratelimit"

func (s *Snapshot) addRateLimits(rls []*ratelimit.RateLimit) {
	for _, r := range rls {
		s.rateLimitsByID[r.Meta.ID] = r
		s.rateLimitsByName[r.Meta.Name] = r
	}
}
