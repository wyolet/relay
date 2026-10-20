// Package tokencount keeps the bytes-to-tokens ratio relay observed on real traffic, so a caller asking "how many input tokens is this prompt" can be answered from measurement when the upstream exposes no counting endpoint.
//
// Every completed generation yields a true pair — the request body's byte length and the input-token count the upstream reported — and the last pair written wins: one ratio per session and one per model, no history, no averaging. A session ratio is the sharper answer (one conversation, one prompt shape); the model ratio is the fallback for a request that carries no session.
//
// A pod writes a ratio only when it moved more than 5% from the one that pod last wrote to the key, or that write is over 5 minutes old, so the stored ratio may lag the latest sample by up to that much. Pods do not share what they wrote: until its refresh, a pod skips a sample close to its own last write even when another pod has since stored a different ratio.
//
// Deliberately out of scope: tokenisation (relay never tokenises — counts come from the provider), per-request accounting (that is usage logging), and any knowledge of which client header carries a session id (the caller passes the key it resolved).
//
// Expected kv ops: Observe = at most 1 Set per key it can write (≤2 per completed request, from the detached post-flight goroutine), and none while the ratio holds steady; Ratio = 1 Get, plus a second Get only when a session key was given and missed.
package tokencount
