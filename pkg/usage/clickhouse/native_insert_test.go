package clickhouse

import (
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/clickhouse-go/v2"
)

func TestNativeInserterTakesDSNOptions(t *testing.T) {
	opts, err := clickhouse.ParseDSN("clickhouse://relay:pw@ch-a:9000,ch-b:9440/ignored?secure=true&compress=zstd&compress_level=3&read_timeout=5s&max_insert_threads=4")
	if err != nil {
		t.Fatal(err)
	}
	n := newNativeInserter(opts, "relay")

	if len(n.dial) != 2 || n.dial[0].Address != "ch-a:9000" || n.dial[1].Address != "ch-b:9440" {
		t.Fatalf("addresses: %+v", n.dial)
	}
	o := n.dial[1]
	if o.Database != "relay" || o.User != "relay" || o.Password != "pw" {
		t.Errorf("auth: database=%q user=%q password=%q", o.Database, o.User, o.Password)
	}
	if o.TLS == nil {
		t.Error("secure=true: TLS not set")
	}
	if o.Compression != ch.CompressionZSTD || o.CompressionLevel != 3 {
		t.Errorf("compression: %v level %d", o.Compression, o.CompressionLevel)
	}
	if o.ReadTimeout != 5*time.Second || o.DialTimeout != defaultDialTimeout {
		t.Errorf("timeouts: read %v dial %v", o.ReadTimeout, o.DialTimeout)
	}
	if len(o.Settings) != 1 || o.Settings[0] != (ch.Setting{Key: "max_insert_threads", Value: "4", Important: true}) {
		t.Errorf("settings: %+v", o.Settings)
	}
	if n.lifetime != time.Hour {
		t.Errorf("lifetime: %v", n.lifetime)
	}
}

func TestNativeInserterDefaultsToUncompressedFraming(t *testing.T) {
	opts, err := clickhouse.ParseDSN("clickhouse://localhost:9000")
	if err != nil {
		t.Fatal(err)
	}
	o := newNativeInserter(opts, "default").dial[0]
	if o.Compression != ch.CompressionNone || o.TLS != nil || o.ReadTimeout != defaultReadTimeout {
		t.Fatalf("defaults: compression %v tls %v read %v", o.Compression, o.TLS, o.ReadTimeout)
	}
}
