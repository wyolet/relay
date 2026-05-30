package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/payload"
	chpayload "github.com/wyolet/relay/pkg/payload/clickhouse"
	payloadfile "github.com/wyolet/relay/pkg/payload/file"
	"github.com/wyolet/relay/pkg/secret"
)

// payloadReaderResolver is the read-side counterpart to the sink Controller:
// it serves the /payloads/* Logs endpoints over whatever backend the
// "payload-logging" settings section currently names. The backend is
// hot-swappable (file ↔ s3), so the reader is rebuilt lazily whenever the
// live config changes — keeping reads pointed at the same store writes go
// to without a restart. The s3 reader is built behind a build tag
// (newS3PayloadReader) so a minimal build carries no cloud SDK.
//
// Reads work regardless of the Enabled toggle: disabling capture stops new
// writes but historical records stay queryable.
type payloadReaderResolver struct {
	src      payloadlog.SettingsSource
	resolver *secret.Registry
	chBoot   payloadCHBoot
	log      *slog.Logger

	mu      sync.Mutex
	applied settings.PayloadLogging
	reader  payloadlog.Reader
	err     error
	has     bool
}

var _ payloadlog.Reader = (*payloadReaderResolver)(nil)

func newPayloadReaderResolver(src payloadlog.SettingsSource, resolver *secret.Registry, chBoot payloadCHBoot, log *slog.Logger) *payloadReaderResolver {
	if log == nil {
		log = slog.Default()
	}
	return &payloadReaderResolver{src: src, resolver: resolver, chBoot: chBoot, log: log}
}

func (p *payloadReaderResolver) Get(ctx context.Context, requestID string) (payloadlog.Record, error) {
	r, err := p.current(ctx)
	if err != nil {
		return payloadlog.Record{}, err
	}
	return r.Get(ctx, requestID)
}

// current returns the reader for the live config, rebuilding only when the
// config changed since the last build. A build error is cached alongside
// the config so a broken backend doesn't rebuild on every request; the next
// config change clears it.
func (p *payloadReaderResolver) current(ctx context.Context) (payloadlog.Reader, error) {
	cfg := p.liveConfig()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.has && cfg == p.applied {
		return p.reader, p.err
	}
	r, err := p.build(ctx, cfg)
	// Release the previous reader's resources (CH holds a connection pool;
	// file/s3 readers are no-ops). Only after a successful rebuild — on
	// error we keep the old reader usable rather than tear it down.
	if err == nil {
		if c, ok := p.reader.(payload.Closer); ok && p.reader != r {
			if cerr := c.Close(); cerr != nil {
				p.log.Warn("payloadlog: closing previous reader", "err", cerr)
			}
		}
		p.applied, p.reader, p.err, p.has = cfg, r, nil, true
	} else {
		p.err, p.has = err, true
		p.applied = cfg
		p.log.Error("payloadlog: reader build failed", "err", err, "backend", cfg.Backend)
	}
	return r, err
}

func (p *payloadReaderResolver) build(ctx context.Context, cfg settings.PayloadLogging) (payloadlog.Reader, error) {
	switch cfg.Backend {
	case "s3":
		return newS3PayloadReader(ctx, cfg, p.resolver)
	case "clickhouse":
		if p.chBoot.DSN == "" {
			return nil, fmt.Errorf("payloadlog: clickhouse backend selected but no CH DSN configured (set RELAY_CH_DSN)")
		}
		return chpayload.NewReader(p.chBoot.config(cfg.CH))
	default: // "file"
		path := cfg.File.Path
		if path == "" {
			path = "relay-payloads.jsonl"
		}
		return payloadfile.NewReader(path), nil
	}
}

func (p *payloadReaderResolver) liveConfig() settings.PayloadLogging {
	v, ok := p.src.Setting(settings.SectionPayloadLogging)
	if !ok {
		return settings.PayloadLogging{}
	}
	cfg, ok := v.(*settings.PayloadLogging)
	if !ok || cfg == nil {
		return settings.PayloadLogging{}
	}
	return *cfg
}
