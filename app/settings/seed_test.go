package settings

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
)

func parseDocs(t *testing.T, yaml string) []manifest.Document {
	t.Helper()
	docs, err := manifest.Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("manifest.Parse: %v", err)
	}
	return docs
}

func TestSectionsFromDocs(t *testing.T) {
	const y = `apiVersion: relay.wyolet.dev/v1alpha2
kind: Setting
metadata:
  name: usage-logging
spec:
  backend: clickhouse
  clickhouse:
    retentionDays: 14
`
	got, err := sectionsFromDocs(parseDocs(t, y))
	if err != nil {
		t.Fatalf("sectionsFromDocs: %v", err)
	}
	raw, ok := got[SectionUsageLogging]
	if !ok {
		t.Fatalf("usage-logging not loaded: %v", got)
	}
	v, err := UsageLoggingDecodeForTest(raw)
	if err != nil {
		t.Fatalf("decode seeded value: %v", err)
	}
	if v.Backend != "clickhouse" || v.CH.RetentionDays != 14 {
		t.Fatalf("seeded value mismatch: %+v", v)
	}
}

func TestSectionsFromDocs_UnknownSection(t *testing.T) {
	const y = `apiVersion: relay.wyolet.dev/v1alpha2
kind: Setting
metadata:
  name: nope-section
spec:
  a: 1
`
	if _, err := sectionsFromDocs(parseDocs(t, y)); err == nil {
		t.Fatal("want error for unknown section")
	}
}

func TestSectionsFromDocs_NonSettingKind(t *testing.T) {
	const y = `apiVersion: relay.wyolet.dev/v1alpha2
kind: Provider
metadata:
  name: openai
spec: {}
`
	if _, err := sectionsFromDocs(parseDocs(t, y)); err == nil {
		t.Fatal("want error for non-Setting kind in settings tree")
	}
}

// UsageLoggingDecodeForTest decodes raw JSON into the typed value via the
// registered section, asserting the seed path produces section-valid JSON.
func UsageLoggingDecodeForTest(raw json.RawMessage) (*UsageLogging, error) {
	sec, _ := Lookup(SectionUsageLogging)
	v, err := sec.Decode(raw)
	if err != nil {
		return nil, err
	}
	return v.(*UsageLogging), nil
}
