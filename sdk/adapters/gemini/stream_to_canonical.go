package gemini

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wyolet/relay/sdk/internal/sse"
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
	currentFCID       string // functionCall.id when Gemini sent one
	currentThoughtSig string // thoughtSignature for the current item, if any
	sigOnEmptyPart    bool   // the current message's signature came on a part of its own with empty text
	// sawFunctionCall records whether any function_call part appeared across
	// the whole stream, so the terminal completion reports finish_reason
	// tool_calls even though Gemini's per-frame finishReason is usually STOP.
	sawFunctionCall bool
}

func (s *geminiToCanonicalStream) translate(chunk []byte) ([]byte, error) {
	var out []byte
	sc := sse.NewScanner(chunk)
	for sc.Next() {
		b, err := s.translateEvent(sc.Data())
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

// geminiStreamFrame is one streamed frame: a generateContent chunk, or an
// {"error":{…}} frame, which would otherwise decode as an empty chunk.
type geminiStreamFrame struct {
	geminiResponse
	Error *struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func (s *geminiToCanonicalStream) translateEvent(data []byte) ([]byte, error) {
	var frame geminiStreamFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return nil, fmt.Errorf("gemini stream: %w", err)
	}
	if frame.Error != nil {
		msg := frame.Error.Message
		if msg == "" {
			msg = string(data)
		}
		errData, _ := json.Marshal(v1.ErrorEvent{
			Code:    frame.Error.Status,
			Message: msg,
		})
		return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventError, Data: errData}}), nil
	}
	gr := &frame.geminiResponse

	var out []byte

	// Emit generation.created on first frame.
	if !s.lifecycleEmitted {
		s.created = time.Now().Unix()
		s.responseID = gr.ResponseID
		if s.responseID == "" {
			s.responseID = fmt.Sprintf("gemini-%d", s.created)
		}
		s.model = gr.ModelVersion
		s.lifecycleEmitted = true

		createdData, _ := json.Marshal(v1.GenerationCreatedEvent{
			ID:    s.responseID,
			Model: s.model,
		})
		out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventGenerationCreated, Data: createdData}})...)
	}

	if len(gr.Candidates) == 0 {
		if promptBlockReason(gr.PromptFeedback) != "" {
			// The blocked-prompt frame is the whole stream; without a terminal
			// event it would end as a silent empty success.
			out = append(out, s.closeCurrentItem()...)
			status, finish, incomplete := promptBlocked()
			out = append(out, s.completedFrame(status, finish, incomplete, gr.UsageMetadata)...)
		}
		return out, nil
	}
	cand := gr.Candidates[0]
	// canonical: safetyRatings dropped on the stream — canonical stream events
	// have no extensions slot; a SAFETY block still surfaces as content_filter
	// via finishReason, and the buffered path carries the ratings.
	if cand.Content == nil {
		// Terminal frame with no content but finishReason/usage.
		if cand.FinishReason != "" || gr.UsageMetadata != nil {
			out = append(out, s.closeCurrentItem()...)
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
			s.currentFCID = p.FunctionCall.ID
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
				CallID:   cmp.Or(p.FunctionCall.ID, geminiCallID(p.FunctionCall.Name, idx)),
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

		} else if p.Thought && (p.Text != "" || p.ThoughtSignature != "") {
			// Reasoning part; an empty-text one carries only a signature.
			if s.currentItemType != v1.ItemTypeReasoning || s.sigOnEmptyPart {
				out = append(out, s.openItem(v1.ItemTypeReasoning, "rs")...)
			}
			if p.ThoughtSignature != "" {
				s.currentThoughtSig = p.ThoughtSignature
			}
			if p.Text == "" {
				continue
			}
			s.thinkBuf.WriteString(p.Text)
			deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
				ItemID: s.currentItemID,
				Index:  s.currentIndex,
				Kind:   v1.DeltaKindReasoning,
				Delta:  p.Text,
			})
			out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemDelta, Data: deltaData}})...)

		} else if !p.Thought && p.Text == "" && p.ThoughtSignature != "" && p.InlineData == nil && p.FileData == nil {
			// A signature-only part signs the open unsigned message or reasoning, or stands alone as a reasoning item.
			// Streamed text is reassembled into one part, which the message's signature rides, as a canonical→Gemini stream sends it.
			open := s.currentItemType == v1.ItemTypeMessage || s.currentItemType == v1.ItemTypeReasoning
			if !open || s.currentThoughtSig != "" {
				out = append(out, s.openItem(v1.ItemTypeReasoning, "rs")...)
			}
			s.currentThoughtSig = p.ThoughtSignature
			s.sigOnEmptyPart = s.currentItemType == v1.ItemTypeReasoning

		} else if !p.Thought && p.Text != "" {
			// Text part. A signed part ends its message, so the signature stays on it.
			if s.currentItemType != v1.ItemTypeMessage || s.currentThoughtSig != "" {
				out = append(out, s.openItem(v1.ItemTypeMessage, "msg")...)
			}
			if p.ThoughtSignature != "" {
				s.currentThoughtSig = p.ThoughtSignature
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

// signatureJSON is the provider_data for the current item's signature, nil when it has none.
func (s *geminiToCanonicalStream) signatureJSON() json.RawMessage {
	switch {
	case s.currentThoughtSig == "":
		return nil
	case s.sigOnEmptyPart:
		return emptyPartSignatureJSON(s.currentThoughtSig)
	}
	return thoughtSignatureJSON(s.currentThoughtSig)
}

// openItem closes the current item and starts a reasoning or message item.
func (s *geminiToCanonicalStream) openItem(itemType v1.ItemType, idPrefix string) []byte {
	out := s.closeCurrentItem()
	idx := s.nextIndex
	s.nextIndex++
	s.currentItemID = fmt.Sprintf("%s_%d", idPrefix, idx)
	s.currentItemType = itemType
	s.currentIndex = idx
	s.thinkBuf.Reset()
	s.textBuf.Reset()
	startData, _ := json.Marshal(v1.ItemStartedEvent{
		ItemID:   s.currentItemID,
		ItemType: itemType,
		Index:    idx,
	})
	return append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemStarted, Data: startData}})...)
}

func (s *geminiToCanonicalStream) closeCurrentItem() []byte {
	if s.currentItemID == "" {
		return nil
	}
	var completedItem v1.Item
	switch s.currentItemType {
	case v1.ItemTypeMessage:
		completedItem = &v1.Message{
			ID:           s.currentItemID,
			Role:         v1.RoleAssistant,
			Status:       v1.StatusCompleted,
			Content:      []v1.Part{&v1.OutputTextPart{Text: s.textBuf.String()}},
			ProviderData: s.signatureJSON(),
		}
	case v1.ItemTypeFunctionCall:
		callID := s.currentFCID
		if callID == "" {
			callID = geminiCallID(s.currentFCName, s.currentIndex)
		}
		fc := &v1.FunctionCall{
			ID:        s.currentItemID,
			CallID:    callID,
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
			Status:  v1.StatusCompleted,
		}
		if r.Content != "" {
			r.Summary = []v1.SummaryText{{Text: r.Content}}
		}
		r.ProviderData = s.signatureJSON()
		completedItem = r
	default:
		return nil
	}
	s.currentThoughtSig = ""
	s.sigOnEmptyPart = false

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
	status, finish, incomplete := geminiFinishReasonToCanonical(finishReason, s.sawFunctionCall)
	return s.completedFrame(status, finish, incomplete, um)
}

func (s *geminiToCanonicalStream) completedFrame(status v1.Status, finish v1.FinishReason, incomplete *v1.IncompleteDetails, um *usageMetadata) []byte {
	gen := v1.GenerationCompletedEvent{
		ID:                s.responseID,
		Status:            status,
		FinishReason:      finish,
		Usage:             geminiUsageToTokens(um),
		IncompleteDetails: incomplete,
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
