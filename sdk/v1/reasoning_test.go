package v1

import "testing"

func TestIsReasoningFrame(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  bool
	}{
		{"reasoning started", "event: item.started\ndata: {\"item_id\":\"r1\",\"item_type\":\"reasoning\",\"index\":0}\n\n", true},
		{"reasoning delta", "event: item.delta\ndata: {\"item_id\":\"r1\",\"kind\":\"reasoning\",\"delta\":\"thinking\"}\n\n", true},
		{"reasoning completed", "event: item.completed\ndata: {\"item_id\":\"r1\",\"item\":{\"type\":\"reasoning\",\"id\":\"r1\",\"summary\":[{\"text\":\"x\"}]}}\n\n", true},
		{"text delta is not reasoning", "event: item.delta\ndata: {\"item_id\":\"m1\",\"kind\":\"text\",\"delta\":\"hi\"}\n\n", false},
		{"message started is not reasoning", "event: item.started\ndata: {\"item_id\":\"m1\",\"item_type\":\"message\",\"index\":0}\n\n", false},
		{"message completed is not reasoning", "event: item.completed\ndata: {\"item_id\":\"m1\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":\"hi\"}}\n\n", false},
		{"generation.completed ignored", "event: generation.completed\ndata: {\"id\":\"g1\",\"status\":\"completed\"}\n\n", false},
		{"non-frame ignored", "\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsReasoningFrame([]byte(tc.frame)); got != tc.want {
				t.Fatalf("IsReasoningFrame = %v, want %v", got, tc.want)
			}
		})
	}
}
