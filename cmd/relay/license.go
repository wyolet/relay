package main

import (
	"context"
	"log/slog"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/license"
)

func loadLicense(bootCtx context.Context, stores *appcatalog.Stores) *license.Service {
	// License: verified offline, never fatal. Resolved before hydrate so
	// the first settings decode already sees the gate. The environment wins
	// over the stored value; a bad or expired one degrades to community.
	licenseSvc := license.New(nil)
	var storedLicense string
	if row, err := stores.Settings.Get(bootCtx, settings.SectionLicense); err == nil {
		if l, ok := row.Value.(*settings.License); ok {
			storedLicense = l.Value
		}
	}
	if info, err := licenseSvc.Set(storedLicense); err != nil {
		slog.Warn("license: unusable — running as community", "err", err)
	} else if info.Licensed {
		slog.Info("license: verified", "customer", info.Customer,
			"expiresAt", info.ExpiresAt, "features", info.Features, "grace", info.Grace)
	}
	settings.SetLicenseGate(licenseSvc)
	return licenseSvc
}

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
