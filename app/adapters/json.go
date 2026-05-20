package adapters

import "encoding/json"

// marshal / unmarshal are tiny helpers so the package's translator impls
// (Identity here, real ones in subpackages) don't each pull encoding/json
// into their already-shape-specific files.
func marshal(v any) ([]byte, error)    { return json.Marshal(v) }
func unmarshal(b []byte, v any) error  { return json.Unmarshal(b, v) }
