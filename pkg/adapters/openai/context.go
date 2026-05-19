package openai

import "context"

type ctxChatRequestKey struct{}

// ContextWithChatRequest returns a new context carrying cr.
func ContextWithChatRequest(ctx context.Context, cr *ChatRequest) context.Context {
	return context.WithValue(ctx, ctxChatRequestKey{}, cr)
}

// ChatRequestFromContext retrieves the ChatRequest stored by ContextWithChatRequest.
// Returns (nil, false) if absent.
func ChatRequestFromContext(ctx context.Context) (*ChatRequest, bool) {
	cr, ok := ctx.Value(ctxChatRequestKey{}).(*ChatRequest)
	return cr, ok && cr != nil
}
