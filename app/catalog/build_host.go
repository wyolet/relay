package catalog

import (
	"github.com/wyolet/relay/pkg/catalogkind/host"
)

// addHosts indexes a copy of each host, so the snapshot never shares a row
// its caller may still edit.
func (s *Snapshot) addHosts(hosts []*host.Host) {
	for _, h := range hosts {
		clean := *h
		s.hostsByID[clean.Meta.ID] = &clean
		s.hostsByName[clean.Meta.Name] = &clean
	}
}
