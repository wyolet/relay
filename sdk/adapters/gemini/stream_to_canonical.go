package gemini

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- NewToCanonicalStream ----

// NewToCanonicalStream returns a stateful per-stream function that converts
// Gemini SSE chunks (streamGenerateContent?alt=sse) into canonical SSE chunks.
func (GeminiTranslator) NewToCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &geminiToCanonicalStream{}
	return s.translate
}

// ---- stream: Gemini → canonical ----

type geminiToCanonicalStream struct {
	responseID       string
	model            string
	created          int64
	lifecycleEmitted bool
	nextIndex        int
	// per-part accumulation (Gemini streams part-by-part within one candidate)
	currentItemID     string
	currentItemType   v1.ItemType
	currentIndex      int
	textBuf           strings.Builder
	argsBuf           strings.Builder
	thinkBuf          strings.Builder
	currentFCName     string
	currentThoughtSig string // thoughtSignature for the current item, if any
	// sawFunctionCall records whether any function_call part appeared across
	// the whole stream, so the terminal completion reports finish_reason
	// tool_calls even though Gemini's per-frame finishReason is usually STOP.
	sawFunctionCall bool
}

func (s *geminiToCanonicalStream) translate(chunk []byte) ([]byte, error) {
	_, data, ok := v1.ParseSSEChunk(chunk)
	if !ok {
		return nil, nil
	}

	var gr geminiResponse
	if err := json.Unmarshal(data, &gr); err != nil {
		// Try error shape.
		var errResp struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		if jerr := json.Unmarshal(data, &errResp); jerr == nil && errResp.Error.Message != "" {
			errData, _ := json.Marshal(v1.ErrorEvent{
				Code:    errResp.Error.Status,
				Message: errResp.Error.Message,
			})
			return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventError, Data: errData}}), nil
		}
		return nil, fmt.Errorf("gemini stream: %w", err)
	}

	var out []byte

	// Emit generation.created on first frame.
	if !s.lifecycleEmitted {
		s.created = time.Now().Unix()
		s.responseID = fmt.Sprintf("gemini-%d", s.created)
		s.model = gr.ModelVersion
		s.lifecycleEmitted = true

		createdData, _ := json.Marshal(v1.GenerationCreatedEvent{
			ID:    s.responseID,
			Model: s.model,
		})
		out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventGenerationCreated, Data: createdData}})...)
	}

	if len(gr.Candidates) == 0 {
		return out, nil
	}
	cand := gr.Candidates[0]
	if cand.Content == nil {
		// Terminal frame with no content but finishReason/usage.
		if cand.FinishReason != "" || gr.UsageMetadata != nil {
			out = append(out, s.emitCompletion(cand.FinishReason, gr.UsageMetadata)...)
		}
		return out, nil
	}

	for _, p := range cand.Content.Parts {
		if p.FunctionCall != nil {
			// Close any open text/thought item.
			out = append(out, s.closeCurrentItem()...)

			idx := s.nextIndex
			s.nextIndex++
			itemID := fmt.Sprintf("fc_%d", idx)
			s.currentItemID = itemID
			s.currentItemType = v1.ItemTypeFunctionCall
			s.currentIndex = idx
			s.currentFCName = p.FunctionCall.Name
			s.currentThoughtSig = p.ThoughtSignature
			s.sawFunctionCall = true
			s.argsBuf.Reset()

			argsStr := "{}"
			if len(p.FunctionCall.Args) > 0 {
				argsStr = string(p.FunctionCall.Args)
			}
			s.argsBuf.WriteString(argsStr)

			startData, _ := json.Marshal(v1.ItemStartedEvent{
				ItemID:   itemID,
				ItemType: v1.ItemTypeFunctionCall,
				Index:    idx,
				Name:     p.FunctionCall.Name,
			})
			out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemStarted, Data: startData}})...)

			deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
				ItemID: itemID,
				Index:  idx,
				Kind:   v1.DeltaKindArguments,
				Delta:  argsStr,
			})
			out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemDelta, Data: deltaData}})...)

			// Function call arrives complete in one frame.
			out = append(out, s.closeCurrentItem()...)

		} else if p.Text != "" && p.Thought {
			// Reasoning part.
			if s.currentItemType != v1.ItemTypeReasoning {
				out = append(out, s.closeCurrentItem()...)
				idx := s.nextIndex
				s.nextIndex++
				itemID := fmt.Sprintf("rs_%d", idx)
				s.currentItemID = itemID
				s.currentItemType = v1.ItemTypeReasoning
				s.currentIndex = idx
				s.currentThoughtSig = p.ThoughtSignature
				s.thinkBuf.Reset()

				startData, _ := json.Marshal(v1.ItemStartedEvent{
					ItemID:   itemID,
					ItemType: v1.ItemTypeReasoning,
					Index:    idx,
				})
				out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemStarted, Data: startData}})...)
			}
			if p.ThoughtSignature != "" {
				s.currentThoughtSig = p.ThoughtSignature
			}
			s.thinkBuf.WriteString(p.Text)
			deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
				ItemID: s.currentItemID,
				Index:  s.currentIndex,
				Kind:   v1.DeltaKindReasoning,
				Delta:  p.Text,
			})
			out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemDelta, Data: deltaData}})...)

		} else if p.Text != "" {
			// Text part.
			if s.currentItemType != v1.ItemTypeMessage {
				out = append(out, s.closeCurrentItem()...)
				idx := s.nextIndex
				s.nextIndex++
				itemID := fmt.Sprintf("msg_%d", idx)
				s.currentItemID = itemID
				s.currentItemType = v1.ItemTypeMessage
				s.currentIndex = idx
				s.textBuf.Reset()

				startData, _ := json.Marshal(v1.ItemStartedEvent{
					ItemID:   itemID,
					ItemType: v1.ItemTypeMessage,
					Index:    idx,
				})
				out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemStarted, Data: startData}})...)
			}
			s.textBuf.WriteString(p.Text)
			deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
				ItemID: s.currentItemID,
				Index:  s.currentIndex,
				Kind:   v1.DeltaKindText,
				Delta:  p.Text,
			})
			out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemDelta, Data: deltaData}})...)
		}
	}

	// Terminal frame: finishReason present means stream is done.
	if cand.FinishReason != "" {
		out = append(out, s.closeCurrentItem()...)
		out = append(out, s.emitCompletion(cand.FinishReason, gr.UsageMetadata)...)
	}

	return out, nil
}

func (s *geminiToCanonicalStream) closeCurrentItem() []byte {
	if s.currentItemID == "" {
		return nil
	}
	var completedItem v1.Item
	switch s.currentItemType {
	case v1.ItemTypeMessage:
		completedItem = &v1.Message{
			ID:      s.currentItemID,
			Role:    v1.RoleAssistant,
			Status:  v1.StatusCompleted,
			Content: []v1.Part{&v1.OutputTextPart{Text: s.textBuf.String()}},
		}
	case v1.ItemTypeFunctionCall:
		fc := &v1.FunctionCall{
			ID:        s.currentItemID,
			CallID:    geminiCallID(s.currentFCName, s.currentIndex),
			Name:      s.currentFCName,
			Arguments: s.argsBuf.String(),
			Status:    v1.StatusCompleted,
		}
		if s.currentThoughtSig != "" {
			fc.ProviderData = thoughtSignatureJSON(s.currentThoughtSig)
		}
		completedItem = fc
	case v1.ItemTypeReasoning:
		r := &v1.Reasoning{
			ID:      s.currentItemID,
			Content: s.thinkBuf.String(),
			Summary: []v1.SummaryText{{Text: s.thinkBuf.String()}},
			Status:  v1.StatusCompleted,
		}
		if s.currentThoughtSig != "" {
			r.ProviderData = thoughtSignatureJSON(s.currentThoughtSig)
		}
		completedItem = r
	default:
		return nil
	}
	s.currentThoughtSig = ""

	completedData, _ := json.Marshal(v1.ItemCompletedEvent{
		ItemID: s.currentItemID,
		Index:  s.currentIndex,
		Item:   completedItem,
	})
	s.currentItemID = ""
	s.currentItemType = ""
	return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemCompleted, Data: completedData}})
}

func (s *geminiToCanonicalStream) emitCompletion(finishReason string, um *usageMetadata) []byte {
	status, finish, _ := geminiFinishReasonToCanonical(finishReason, s.sawFunctionCall)

	gen := v1.GenerationCompletedEvent{
		ID:           s.responseID,
		Status:       status,
		FinishReason: finish,
		Usage:        geminiUsageToTokens(um),
	}
	completedData, _ := json.Marshal(gen)
	return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventGenerationCompleted, Data: completedData}})
}

// ---- shared helpers ----

func geminiSSEBytes(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return geminiSSEBytesRaw(b), nil
}

func geminiSSEBytesRaw(data []byte) []byte {
	var sb strings.Builder
	sb.WriteString("data: ")
	sb.Write(data)
	sb.WriteString("\n\n")
	return []byte(sb.String())
}

func marshalCanonFrames(frames []v1.SSEFrame) []byte {
	var buf []byte
	for _, f := range frames {
		buf = append(buf, f.Bytes()...)
	}
	return buf
}
