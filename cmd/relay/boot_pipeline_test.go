package main

import (
	"testing"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/internal/config"
)

func TestUpstreamMaxIdlePerHost(t *testing.T) {
	for _, tc := range []struct {
		name           string
		idle, inflight int
		want           int
	}{
		{"set", 64, 500, 64},
		{"follows in-flight cap", 0, 500, 500},
		{"follows default in-flight cap", 0, 0, httpapi.DefaultMaxInflight},
	} {
		cfg := &config.Config{UpstreamMaxIdlePerHost: tc.idle, MaxInflight: tc.inflight}
		if got := upstreamMaxIdlePerHost(cfg); got != tc.want {
			t.Errorf("%s: upstreamMaxIdlePerHost = %d, want %d", tc.name, got, tc.want)
		}
	}
}
