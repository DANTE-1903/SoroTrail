// Package requestid carries the correlation id that ties every log line,
// store query, and RPC call back to the unit of work that caused it.
//
// The id lives on the context rather than in method signatures: the tracing
// decorators already thread a context through the same layers, so the
// request id rides along for free and a layer can read it without every
// caller above learning a new parameter.
//
// One field name — [Field] — is used for both HTTP requests and background
// jobs. An HTTP request carries the value the request-id middleware
// assigned; a background worker carries its stable job id. Either way a
// single value identifies the originating work, so one grep across a log
// stream finds everything a request or a job touched.
package requestid

import "context"

// Field is the structured-log key carrying the correlation id. HTTP request
// logs and background-worker logs both write it.
const Field = "request_id"

// Stable job ids for the background workers. They are constants rather than
// per-run uuids because the originating component — not an opaque token — is
// what lets an operator correlate a worker's own log lines with the store
// and RPC activity it drove.
const (
	JobIngester = "ingester"
	JobPruner   = "pruner"
	JobAuditor  = "auditor"
	JobWebhook  = "webhook"
)

// ctxKey is the context key type for the request id. It is unexported so no
// other package can collide with the entry.
type ctxKey struct{}

// WithRequestID returns a copy of ctx carrying id. An empty id is ignored so
// a missing value never masks one installed further up the stack.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, id)
}

// WithJob returns a copy of ctx carrying a background job's stable id. It is
// a named alias for [WithRequestID] so a worker's intent reads clearly at the
// call site.
func WithJob(ctx context.Context, jobID string) context.Context {
	return WithRequestID(ctx, jobID)
}

// FromContext returns the correlation id carried by ctx, or "" when the
// context did not originate from a request or an instrumented job.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// Attrs returns the slog key/value pair for ctx's correlation id, or nil
// when there is none. Callers append the result to an existing attribute
// slice, so a log line gains the field only when it is meaningful.
func Attrs(ctx context.Context) []any {
	if id := FromContext(ctx); id != "" {
		return []any{Field, id}
	}
	return nil
}
