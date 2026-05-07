package anthropic

import (
	"github.com/wyolet/relay/internal/pipeline"
	pkganthropic "github.com/wyolet/relay/pkg/api/anthropic"
)

func init() {
	pipeline.RegisterStreamTransformerFactory("anthropic", "openai", func() func([]byte) ([]byte, error) {
		t := &pkganthropic.OpenAIToAnthropic{}
		return t.TransformChunk
	})
	pipeline.RegisterStreamTransformerFactory("openai", "anthropic", func() func([]byte) ([]byte, error) {
		t := &pkganthropic.AnthropicToOpenAI{}
		return t.TransformChunk
	})
}
