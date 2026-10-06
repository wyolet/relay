package settings

import "testing"

func TestDecodePayloadLogging_Retention(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{"absent keeps the default", `{"backend":"clickhouse"}`, DefaultPayloadRetentionDays, false},
		{"zero keeps forever", `{"backend":"clickhouse","retentionDays":0}`, 0, false},
		{"explicit days", `{"backend":"clickhouse","retentionDays":7}`, 7, false},
		{"negative rejected", `{"backend":"clickhouse","retentionDays":-1}`, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := decodePayloadLogging([]byte(c.raw))
			if c.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			p := v.(*PayloadLogging)
			if p.RetentionDays != c.want {
				t.Fatalf("RetentionDays = %d, want %d", p.RetentionDays, c.want)
			}
		})
	}
}

func TestAdminOnly_RetentionSections(t *testing.T) {
	for _, s := range []string{SectionAudit, SectionUsageLogging, SectionPayloadLogging} {
		if !AdminOnly(s) {
			t.Errorf("AdminOnly(%q) = false, want true", s)
		}
	}
}
