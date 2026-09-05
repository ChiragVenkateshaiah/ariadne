// Package shared provides the plumbing every SUT service needs: structured
// logging, trace propagation, and a health handler. Kept deliberately tiny --
// these are demo services, not a platform.
//
// Every log line is JSON on stdout. This is not cosmetic: the Evidence
// Correlator reads exactly this stream back out of the pods (LogCollector's
// PodLogs -> LogLine, see proto/ariadne/v1/evidence.proto) and an LLM reasons
// over it to produce a root-cause narrative. A log line with no trace_id or
// no structured fields is a log line the correlator cannot stitch across
// services -- so treat this logger as part of the product, not an afterthought.
package shared

import (
	"context"
	"log/slog"
	"net/http"
	"os"
)

type ctxKey int

const traceIDKey ctxKey = iota

// NewLogger returns a JSON slog.Logger writing to stdout, tagged with the
// service name so the correlator can filter a multi-service log bundle down
// to "what did pricing-svc say" without relying on pod name parsing.
func NewLogger(service string) *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h).With("service", service)
}

// TraceID pulls the request's trace id out of context, if the middleware set one.
func TraceID(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey).(string); ok {
		return v
	}
	return ""
}

func withTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey, id)
}

// LoggerFromRequest returns a logger pre-bound with the request's trace id,
// so every log line inside a handler carries it without manual plumbing.
func LoggerFromRequest(base *slog.Logger, r *http.Request) *slog.Logger {
	return base.With("trace_id", TraceID(r.Context()))
}
