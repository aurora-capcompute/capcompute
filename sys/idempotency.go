package sys

import "context"

type idempotencyKeyContextKey struct{}

// WithIdempotencyKey attaches the intent identity of the in-flight syscall —
// (run, position, call-hash) — to the dispatch context. The replay layer sets
// it before delegating; it is stable across crash-retries of the same intent.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, idempotencyKeyContextKey{}, key)
}

// IdempotencyKey returns the intent identity for the in-flight syscall.
// Drivers performing effects should hand it to the effect side (or dedup on
// it) so an at-least-once retry of an open intent does not double-execute.
func IdempotencyKey(ctx context.Context) (string, bool) {
	key, ok := ctx.Value(idempotencyKeyContextKey{}).(string)
	return key, ok
}
