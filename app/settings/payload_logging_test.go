package settings

import "testing"

func TestDecodePayloadLogging_Retention(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    int
		wantErr string
	}{
		{"absent keeps the default", `{"backend":"clickhouse"}`, DefaultPayloadRetentionDays, ""},
		{"zero keeps forever", `{"backend":"clickhouse","retentionDays":0}`, 0, ""},
		{"explicit days", `{"backend":"clickhouse","retentionDays":7}`, 7, ""},
		{"a disabled section may hold partial config", `{"backend":"tape","maxBytes":-1,"retentionDays":3}`, 3, ""},
		{"negative rejected", `{"backend":"clickhouse","retentionDays":-1}`, 0, "payload-logging: retentionDays must be >= 0"},
		{"negative body cap rejected", `{"enabled":true,"backend":"clickhouse","maxBytes":-1}`, 0, "payload-logging: maxBytes must be >= 0"},
		{"unknown backend rejected", `{"enabled":true,"backend":"tape"}`, 0, `payload-logging: backend must be "file", "s3", or "clickhouse", got "tape"`},
		{"s3 without a bucket rejected", `{"enabled":true,"backend":"s3","s3":{"useSSL":true}}`, 0, "payload-logging: s3 backend requires s3.bucket"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := decodePayloadLogging([]byte(c.raw))
			if got := errMsg(err); got != c.wantErr {
				t.Fatalf("decode = %q, want %q", got, c.wantErr)
			}
			if err != nil {
				if v != nil {
					t.Fatalf("a refused section decoded to %+v", v)
				}
				return
			}
			if p := v.(*PayloadLogging); p.RetentionDays != c.want {
				t.Fatalf("RetentionDays = %d, want %d", p.RetentionDays, c.want)
			}
		})
	}
}

func TestDecodePayloadLogging_Dedup(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"backend":"clickhouse"}`:                             false,
		`{"backend":"clickhouse","clickhouse":{"dedup":true}}`: true,
	} {
		v, err := decodePayloadLogging([]byte(raw))
		if err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if got := v.(*PayloadLogging).CH.Dedup; got != want {
			t.Fatalf("decode %s: CH.Dedup = %v, want %v", raw, got, want)
		}
	}
}

func TestAdminOnly_RetentionSections(t *testing.T) {
	for _, s := range []string{SectionAudit, SectionUsageLogging, SectionPayloadLogging} {
		if !AdminOnly(s) {
			t.Errorf("AdminOnly(%q) = false, want true", s)
		}
	}
}
