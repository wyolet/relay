package control

import "testing"

func TestPayloadFilter_ToQuery(t *testing.T) {
	in := payloadFilterInput{
		Since:     "24h",
		PolicyID:  []string{"p1", "p2"},
		ModelID:   []string{"m1"},
		ErrorKind: []string{"upstream_error"},
		StatusMin: 400,
	}
	q, err := in.toQuery()
	if err != nil {
		t.Fatalf("toQuery: %v", err)
	}
	if q.Since.Hours() != 24 {
		t.Fatalf("since: got %v", q.Since)
	}
	if len(q.PolicyID) != 2 || q.ModelID[0] != "m1" || q.StatusMin != 400 {
		t.Fatalf("filters not mapped: %+v", q)
	}
	if q.ErrorKind[0] != "upstream_error" {
		t.Fatalf("error_kind not mapped: %+v", q)
	}
}

func TestPayloadFilter_ToQuery_RejectsInvertedWindow(t *testing.T) {
	in := payloadFilterInput{From: "2026-05-30T10:00:00Z", To: "2026-05-30T09:00:00Z"}
	if _, err := in.toQuery(); err == nil {
		t.Fatal("want error when to < from")
	}
}

func TestPayloadFilter_ToQuery_BadTime(t *testing.T) {
	if _, err := (payloadFilterInput{From: "not-a-time"}).toQuery(); err == nil {
		t.Fatal("want error for malformed from")
	}
}
