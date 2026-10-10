package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func TestParseResponse_KeepsResponseID(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],"responseId":"resp-abc"}`
	resp, err := tr.ParseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "resp-abc" {
		t.Fatalf("id = %q, want resp-abc", resp.ID)
	}
	wire, err := tr.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"responseId":"resp-abc"`) {
		t.Errorf("responseId not serialized: %s", wire)
	}
}

func TestToCanonicalStream_KeepsResponseID(t *testing.T) {
	chunk := []byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],"responseId":"resp-abc"}` + "\n\n")
	canon, err := tr.NewToCanonicalStream()(chunk)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{v1.EventGenerationCreated, v1.EventGenerationCompleted} {
		if !strings.Contains(string(canon), "event: "+ev+"\ndata: {\"id\":\"resp-abc\"") {
			t.Errorf("%s does not carry the upstream id:\n%s", ev, canon)
		}
	}

	from := tr.NewFromCanonicalStream()
	var wire []byte
	for _, f := range strings.SplitAfter(string(canon), "\n\n") {
		if f == "" {
			continue
		}
		b, err := from([]byte(f))
		if err != nil {
			t.Fatal(err)
		}
		wire = append(wire, b...)
	}
	if !strings.Contains(string(wire), `"responseId":"resp-abc"`) {
		t.Errorf("responseId not serialized on the stream: %s", wire)
	}
}

// A call id minted by another vendor carries no function name; the name must come from the matching call earlier in the request.
func TestSerializeRequest_FunctionResponse_NameFromForeignCallID(t *testing.T) {
	req := &v1.Request{
		Model: v1.ModelRefs{"gemini-2.5-flash"},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "weather?"}}},
			&v1.FunctionCall{CallID: "toolu_01ABC", Name: "get_weather", Arguments: `{"city":"Paris"}`},
			&v1.FunctionCallOutput{CallID: "toolu_01ABC", Output: `{"temp":21}`},
		},
	}
	body, err := tr.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var gr geminiRequest
	if err := json.Unmarshal(body, &gr); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range gr.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				names = append(names, p.FunctionResponse.Name)
			}
		}
	}
	if len(names) != 1 || names[0] != "get_weather" {
		t.Errorf("functionResponse names = %v, want [get_weather]", names)
	}
}
