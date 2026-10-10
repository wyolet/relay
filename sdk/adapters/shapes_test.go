// Package adapters_test holds the cross-vendor translator tests: the golden
// corpus under testdata/<vendor>/<case>, the rule-11 drop audit, and the
// split-frame stream check. It is test-only so the vendor adapters keep never
// importing each other; nothing outside this directory depends on it.
package adapters_test

import (
	"strings"

	"github.com/wyolet/relay/sdk/adapters/anthropic"
	"github.com/wyolet/relay/sdk/adapters/gemini"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// shape is one wire shape under test. dir is both the testdata vendor folder
// and the adapter package folder whose sources hold its drop annotations;
// casePrefix picks the shape's cases when a vendor owns several shapes.
type shape struct {
	name       string
	dir        string
	casePrefix string
	tr         v1.Translator
}

var shapes = []shape{
	{name: "openai-chat", dir: "openai", casePrefix: "chat-", tr: openai.CCTranslator{}},
	{name: "openai-responses", dir: "openai", casePrefix: "responses-", tr: openai.ResponsesTranslator{}},
	{name: "anthropic", dir: "anthropic", tr: anthropic.AnthropicTranslator{}},
	{name: "gemini", dir: "gemini", tr: gemini.GeminiTranslator{}},
}

// shapeForCase returns the shape owning testdata/<dir>/<caseName>.
func shapeForCase(dir, caseName string) (shape, bool) {
	for _, s := range shapes {
		if s.dir == dir && strings.HasPrefix(caseName, s.casePrefix) {
			return s, true
		}
	}
	return shape{}, false
}

func shapeByName(name string) shape {
	for _, s := range shapes {
		if s.name == name {
			return s
		}
	}
	panic("unknown shape " + name)
}
