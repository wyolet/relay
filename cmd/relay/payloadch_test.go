package main

import (
	"testing"

	"github.com/wyolet/relay/app/settings"
	chpayload "github.com/wyolet/relay/pkg/payload/clickhouse"
)

func TestPayloadCHBootConfig(t *testing.T) {
	boot := payloadCHBoot{DSN: "clickhouse://ch:9000/relay", WALDir: "boot-wal"}
	got := boot.config(settings.PayloadLogging{
		RetentionDays: 7,
		CH:            settings.PayloadClickHouse{WALDir: "section-wal", Dedup: true},
	})
	want := chpayload.Config{DSN: boot.DSN, RetentionDays: 7, WALDir: "section-wal", Dedup: true}
	if got != want {
		t.Fatalf("config = %+v, want %+v", got, want)
	}
}
