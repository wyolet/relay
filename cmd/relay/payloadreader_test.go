package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/settings/settingstest"
)

func TestPayloadReaderResolverEraseUnsupportedBackend(t *testing.T) {
	cfg := settings.PayloadLogging{Backend: "file"}
	cfg.File.Path = filepath.Join(t.TempDir(), "payloads.jsonl")
	src := settingstest.Sections(map[string]any{settings.SectionPayloadLogging: &cfg})
	r := newPayloadReaderResolver(src, nil, payloadCHBoot{}, nil)

	_, err := r.Erase(context.Background(), payloadlog.EraseFilter{ProjectID: "p-1"})
	if !errors.Is(err, payloadlog.ErrEraseUnsupported) || !strings.Contains(err.Error(), `"file"`) {
		t.Fatalf("err = %v, want ErrEraseUnsupported naming the file backend", err)
	}
}
