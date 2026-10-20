package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyolet/relay/sdk/internal/sse"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// refTranslate is translate as it stood when each chunk was decoded three
// times (an error probe, ChatStreamChunk, a reasoning probe) and frames were
// joined from v1.SSEFrame.Bytes. FuzzCCStreamToCanonical holds translate to it.
func (s *ccToCanonicalStream) refTranslate(chunk []byte) ([]byte, error) {
	var out []byte
	sc := sse.NewScanner(chunk)
	for sc.Next() {
		b, err := s.refTranslateEvent(sc.Data())
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = b
		} else {
			out = append(out, b...)
		}
	}
	return out, nil
}

func (s *ccToCanonicalStream) refTranslateEvent(data []byte) ([]byte, error) {
	if s.errorEmitted {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return s.handleDone()
	}
	if errFrame, ok := refStreamErrorFrame(data); ok {
		s.errorEmitted = true
		return refMarshalFrames([]v1.SSEFrame{errFrame}), nil
	}
	var ccChunk ChatStreamChunk
	if err := json.Unmarshal(data, &ccChunk); err != nil {
		return nil, fmt.Errorf("cc stream: parse chunk: %w", err)
	}
	if ccChunk.Usage != nil {
		s.lastUsage = ccChunk.Usage
	}
	if ccChunk.ServiceTier != "" {
		s.serviceTier = ccChunk.ServiceTier
	}
	var frames []v1.SSEFrame
	if !s.lifecycleEmitted {
		s.responseID = ccChunk.ID
		s.model = ccChunk.Model
		s.created = ccChunk.Created
		if s.created == 0 {
			s.created = time.Now().Unix()
		}
		if s.responseID == "" {
			s.responseID = fmt.Sprintf("resp_%d", s.created)
		}
		if s.toolItems == nil {
			s.toolItems = make(map[int]*ccStreamItem)
		}
		createdData, _ := json.Marshal(v1.GenerationCreatedEvent{ID: s.responseID, Model: s.model})
		frames = append(frames, v1.SSEFrame{Event: v1.EventGenerationCreated, Data: createdData})
		s.lifecycleEmitted = true
	}
	if len(ccChunk.Choices) == 0 {
		return refMarshalFrames(frames), nil
	}
	ch := ccChunk.Choices[0]
	delta := ch.Delta
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		s.status, s.finishReason, s.incomplete = ccFinishReasonToCanonical(*ch.FinishReason)
		s.finishSeen = true
	}
	if rc, field := refExtractReasoningContent(data); rc != "" {
		rf, err := s.handleReasoningDelta(rc, field)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}
	if delta.Content != nil && *delta.Content != "" {
		rf, err := s.handleTextDelta(*delta.Content)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}
	for _, tc := range delta.ToolCalls {
		rf, err := s.handleToolCallDelta(tc)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}
	if delta.Refusal != nil && *delta.Refusal != "" {
		s.refused = true
		rf, err := s.handleTextDelta(*delta.Refusal)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}
	return refMarshalFrames(frames), nil
}

func refStreamErrorFrame(data []byte) (v1.SSEFrame, bool) {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return v1.SSEFrame{}, false
	}
	errRaw := bytes.TrimSpace(probe.Error)
	if len(errRaw) == 0 || bytes.Equal(errRaw, []byte("null")) {
		return v1.SSEFrame{}, false
	}
	var ccErr struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	}
	_ = json.Unmarshal(errRaw, &ccErr)
	code := ccErr.Type
	if code == "" && len(ccErr.Code) > 0 {
		var codeString string
		if err := json.Unmarshal(ccErr.Code, &codeString); err == nil {
			code = codeString
		} else {
			code = string(bytes.TrimSpace(ccErr.Code))
		}
	}
	msg := ccErr.Message
	if msg == "" {
		msg = string(data)
	}
	errData, _ := json.Marshal(v1.ErrorEvent{Code: code, Message: msg})
	return v1.SSEFrame{Event: v1.EventError, Data: errData}, true
}

func refExtractReasoningContent(raw []byte) (text, field string) {
	var probe struct {
		Choices []struct {
			Delta struct {
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || len(probe.Choices) == 0 {
		return "", ""
	}
	d := probe.Choices[0].Delta
	if d.ReasoningContent != "" {
		return d.ReasoningContent, ccReasoningFieldStd
	}
	if d.Reasoning != "" {
		return d.Reasoning, ccReasoningFieldOllama
	}
	return "", ""
}

func refMarshalFrames(frames []v1.SSEFrame) []byte {
	var buf []byte
	for _, f := range frames {
		buf = append(buf, f.Bytes()...)
	}
	return buf
}

func TestMarshalCanonicalFrames(t *testing.T) {
	cases := [][]v1.SSEFrame{
		nil,
		{},
		{{Event: v1.EventItemDelta, Data: []byte(`{"a":1}`)}},
		{{Data: []byte(`{}`)}, {Event: v1.EventError}, {Event: "x", Data: []byte("y")}},
	}
	for i, frames := range cases {
		got, want := marshalCanonicalFrames(frames), refMarshalFrames(frames)
		if !bytes.Equal(got, want) || (got == nil) != (want == nil) {
			t.Errorf("case %d: got %q, want %q", i, got, want)
		}
	}
}

// ccStreamEquivalenceSeeds cover what the recorded streams lack: error frames
// in each code shape, Ollama reasoning, invalid and mistyped JSON, and
// duplicate keys.
var ccStreamEquivalenceSeeds = []string{
	"data: {\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}\n\n",
	"data: {\"error\":{\"message\":\"slow down\",\"code\":\"rate_limit_exceeded\"}}\n\n",
	"data: {\"error\":{\"code\":429}}\n\n",
	"data: {\"error\":\"boom\"}\n\n",
	"data: {\"error\":null,\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n",
	"data: {\"error\":{\"message\":\"x\"},\"created\":\"not a number\"}\n\n",
	"data: {\"choices\":[{\"delta\":{\"reasoning\":\"think\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n",
	"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"a\",\"reasoning\":\"b\"}}]}\n\ndata: [DONE]\n\n",
	"data: {\"choices\":[{\"delta\":{\"reasoning_content\":5,\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n",
	"data: {\"choices\":[{\"delta\":{\"reasoning\":\"r\"}},{\"delta\":{\"reasoning\":{}}}]}\n\ndata: [DONE]\n\n",
	"data: {\"choices\":[{\"delta\":{\"reasoning\":\"r\"}},{\"delta\":{\"reasoning\":1}}],\"choices\":[{\"delta\":{}}]}\n\ndata: [DONE]\n\n",
	"data: {\"choices\":[{\"delta\":{\"REASONING\":\"r\",\"reasoning\":null}}]}\n\ndata: [DONE]\n\n",
	"data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":\"st\n\ndata: [DONE]\n\n",
	"data: [1,2]\n\ndata: \"x\"\n\ndata: null\n\ndata: \n\ndata: [DONE]\n\n",
	"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n",
	"data: {\"choices\":[{\"delta\":{\"refusal\":\"no\"},\"finish_reason\":\"stop\"}],\"service_tier\":\"flex\",\"usage\":{\"prompt_tokens\":2}}\n\ndata: [DONE]\n\n",
}

// FuzzCCStreamToCanonical checks that translate gives the same bytes and the
// same error as refTranslate on any frame sequence.
func FuzzCCStreamToCanonical(f *testing.F) {
	streams, err := filepath.Glob(filepath.Join("..", "testdata", "openai", "chat-*", "stream.sse"))
	if err != nil || len(streams) == 0 {
		f.Fatalf("no recorded streams: %v", err)
	}
	for _, path := range streams {
		body, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(body)
	}
	for _, s := range ccStreamEquivalenceSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		got, want := &ccToCanonicalStream{}, &ccToCanonicalStream{}
		sc := bufio.NewScanner(bytes.NewReader(body))
		sc.Buffer(nil, len(body)+1)
		sc.Split(v1.SplitSSEFrames)
		var frames [][]byte
		for sc.Scan() {
			frames = append(frames, append(sc.Bytes(), "\n\n"...))
		}
		frames = append(frames, []byte("data: [DONE]\n\n"))
		for i, frame := range frames {
			// A chunk without created stamps the clock into the fallback id.
			sec := time.Now().Unix()
			gotOut, gotErr := got.translate(frame)
			wantOut, wantErr := want.refTranslate(frame)
			if time.Now().Unix() != sec {
				return
			}
			if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || !bytes.Equal(gotOut, wantOut) || (gotOut == nil) != (wantOut == nil) {
				t.Fatalf("frame %d %q:\n got %q (%v)\nwant %q (%v)", i, frame, gotOut, gotErr, wantOut, wantErr)
			}
		}
	})
}
