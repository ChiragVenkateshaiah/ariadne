package shared

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const TraceHeader = "X-Trace-Id"

// WithTracing propagates X-Trace-Id across service calls (generating one if
// this is the entry point) and logs every request/response. This is the
// mechanism behind LogLine.trace_id in the evidence proto: it is what lets the
// correlator stitch "web-ui got a 500" to "payment-svc threw at 14:32:07.331"
// as the same causal chain instead of two unrelated log lines.
func WithTracing(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := r.Header.Get(TraceHeader)
		if traceID == "" {
			traceID = uuid.NewString()
		}
		w.Header().Set(TraceHeader, traceID)
		r = r.WithContext(withTraceID(r.Context(), traceID))

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic in handler",
					"trace_id", traceID, "method", r.Method, "path", r.URL.Path,
					"panic", rec)
				sw.WriteHeader(http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(sw, r)

		logger.Info("request",
			"trace_id", traceID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// PropagateTraceHeader copies the inbound trace id onto an outbound request,
// used by every service-to-service HTTP client in the SUT.
func PropagateTraceHeader(req *http.Request, traceID string) {
	if traceID != "" {
		req.Header.Set(TraceHeader, traceID)
	}
}
