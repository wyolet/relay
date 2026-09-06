package main

import (
	"log/slog"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/license"
)

// applyLicenseSection makes a stored license live. An installed license has
// to reach every pod, and the settings section is the only thing that
// travels — so the watcher, not the PUT handler, is what activates it.
func applyLicenseSection(svc *license.Service) func(settings.License) {
	return func(l settings.License) {
		if _, err := svc.Set(l.Value); err != nil {
			slog.Warn("license: stored value unusable — running as community", "err", err)
		}
	}
}
