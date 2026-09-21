// Package tokencount keeps the bytes-to-tokens ratio relay observed on real traffic, so a caller asking "how many input tokens is this prompt" can be answered from measurement when the upstream exposes no counting endpoint.
//
// Every completed generation yields a true pair — the request body's byte length and the input-token count the upstream reported — and the last pair wins: one ratio per session and one per model, no history, no averaging. A session ratio is the sharper answer (one conversation, one prompt shape); the model ratio is the fallback for a request that carries no session.
//
// Deliberately out of scope: tokenisation (relay never tokenises — counts come from the provider), per-request accounting (that is usage logging), and any knowledge of which client header carries a session id (the caller passes the key it resolved).
//
// Expected kv ops: Observe = 1 Set per key it can write (≤2 per completed request, from the detached post-flight goroutine); Ratio = 1 Get, plus a second Get only when a session key was given and missed.
package tokencount
