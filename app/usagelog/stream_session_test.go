package usagelog

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/sdk/adapters/anthropic"
	"github.com/wyolet/relay/sdk/adapters/gemini"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

const recordedStreamsDir = "../../sdk/adapters/testdata"

func translatorForCase(vendor, name string) v1.Translator {
	switch {
	case vendor == "anthropic":
		return anthropic.AnthropicTranslator{}
	case vendor == "gemini":
		return gemini.GeminiTranslator{}
	case vendor == "openai" && strings.HasPrefix(name, "chat-"):
		return openai.CCTranslator{}
	case vendor == "openai" && strings.HasPrefix(name, "responses-"):
		return openai.ResponsesTranslator{}
	}
	return nil
}

// framesLikeSession splits body exactly as StreamSession.Write + Finish do.
func framesLikeSession(body []byte) [][]byte {
	var frames [][]byte
	buf := body
	for {
		n, frame, _ := v1.SplitSSEFrames(buf, false)
		if n == 0 {
			break
		}
		if len(bytes.TrimSpace(frame)) > 0 {
			frames = append(frames, v1.NormalizeSSELineEnds(frame))
		}
		buf = buf[n:]
	}
	if len(bytes.TrimSpace(buf)) > 0 {
		frames = append(frames, v1.NormalizeSSELineEnds(buf))
	}
	return frames
}

// The usage observer reads the session's Summary instead of running its own
// summarizer; the usage Event and the rate-limit tokens must still equal what
// an independent summarizer fed the same frames yields, however the bytes are
// chunked on the way in.
func TestStreamSession_UsageMatchesIndependentSummarizer(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(recordedStreamsDir, "*", "*", "stream.sse"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no recorded streams under %s: %v", recordedStreamsDir, err)
	}
	feeds := map[string]func(*lifecycle.StreamSession, []byte){
		"frames": func(s *lifecycle.StreamSession, body []byte) {
			for _, f := range framesLikeSession(body) {
				s.Observe(f)
			}
		},
		"whole": func(s *lifecycle.StreamSession, body []byte) { _, _ = s.Write(body) },
		"byte":  func(s *lifecycle.StreamSession, body []byte) { writeChunks(s, body, 1) },
		"7byte": func(s *lifecycle.StreamSession, body []byte) { writeChunks(s, body, 7) },
	}

	var withTokens, withTier int
	for _, path := range paths {
		caseDir := filepath.Dir(path)
		vendor, name := filepath.Base(filepath.Dir(caseDir)), filepath.Base(caseDir)
		tr := translatorForCase(vendor, name)
		if tr == nil {
			t.Fatalf("%s/%s: no translator", vendor, name)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		ref := v1.NewStreamSummarizer(tr)
		for _, f := range framesLikeSession(body) {
			ref.Observe(f)
		}
		want := ref.Summary()
		if len(want.Tokens) > 0 {
			withTokens++
		}
		if want.ServiceTier != "" {
			withTier++
		}

		for mode, feed := range feeds {
			label := vendor + "/" + name + " " + mode
			reg := lifecycle.New()
			reg.RegisterStreamObserver(NewStreamUsageFactory(nil, ""))
			lc := lifecycle.NewContext("req", "pipeline", time.Now())
			lc.Streamed = true
			lc.Translator = tr
			sess := reg.NewStreamSession(lc)
			feed(sess, body)
			sess.Finish()

			v, ok := lc.Collected(Namespace)
			if !ok {
				t.Fatalf("%s: no usage event", label)
			}
			ev := v.(*Event)
			if !reflect.DeepEqual(ev.Tokens, want.Tokens) {
				t.Errorf("%s: event tokens %v, want %v", label, ev.Tokens, want.Tokens)
			}
			if ev.FinishReason != string(want.FinishReason) {
				t.Errorf("%s: finish %q, want %q", label, ev.FinishReason, want.FinishReason)
			}
			if got := ev.Extras[ExtrasKeyServiceTier]; got != want.ServiceTier {
				t.Errorf("%s: service tier %q, want %q", label, got, want.ServiceTier)
			}
			limitTokens, ok := lc.StreamTokens()
			if !ok || !reflect.DeepEqual(limitTokens, map[string]int64(want.Tokens)) {
				t.Errorf("%s: rate-limit tokens %v (set=%v), want %v", label, limitTokens, ok, want.Tokens)
			}
		}
	}
	// Guard against a corpus or translator change that leaves the comparison vacuous.
	if withTokens < len(paths)/2 || withTier == 0 {
		t.Fatalf("recorded streams carry too little usage: %d/%d with tokens, %d with a service tier", withTokens, len(paths), withTier)
	}
}

func writeChunks(s *lifecycle.StreamSession, body []byte, size int) {
	for len(body) > 0 {
		n := min(size, len(body))
		_, _ = s.Write(body[:n])
		body = body[n:]
	}
}
