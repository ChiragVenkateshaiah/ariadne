package logs

import "github.com/prometheus/client_golang/prometheus"

var (
	evidenceRequestsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ariadne_logcollector_evidence_requests_total",
		Help: "CollectEvidence calls served.",
	})

	auditQueryDurationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "ariadne_logcollector_audit_query_duration_seconds",
		Help:    "Time to scan and filter the audit log for one QueryAuditLog/CollectEvidence call.",
		Buckets: prometheus.DefBuckets,
	})
)

func init() {
	prometheus.MustRegister(evidenceRequestsTotal, auditQueryDurationSeconds)
}
