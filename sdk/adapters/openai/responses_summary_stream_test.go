package openai

import "testing"

func TestResponsesSummaryFrame(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  bool
	}{
		{"created", "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}", false},
		{"text delta", "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"response.completed\"}", false},
		{"refusal delta", "event: response.refusal.delta\ndata: {\"delta\":\"no\"}", false},
		{"item done", "event: response.output_item.done\ndata: {\"item\":{\"type\":\"message\"}}", false},
		{"completed", "event: response.completed\ndata: {\"response\":{\"usage\":{}}}", true},
		{"completed without space", "event:response.completed\ndata:{\"response\":{}}", true},
		{"completed CRLF", "event: response.completed\r\ndata: {\"response\":{}}", true},
		{"incomplete", "event: response.incomplete\ndata: {\"response\":{}}", true},
		{"failed", "event: response.failed\ndata: {\"response\":{}}", true},
		{"error", "event: error\ndata: {\"code\":\"x\",\"message\":\"y\"}", true},
		{"no event name", "data: {\"type\":\"response.completed\"}", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, affects := ResponsesTranslator{}.NewSummaryStream()
			if got := affects([]byte(tc.frame)); got != tc.want {
				t.Errorf("affectsSummary = %v, want %v", got, tc.want)
			}
		})
	}
}
