package openai

import "github.com/wyolet/relay/app/adapters"

// Translator is the OpenAI shape's translator — identity, because OpenAI
// is the canonical hub. Exposed as a named type so callers can be
// explicit, but mechanically it's adapters.Identity.
type Translator struct{ adapters.Identity }
