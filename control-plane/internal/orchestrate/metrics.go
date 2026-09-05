package orchestrate

import "github.com/prometheus/client_golang/prometheus"

var (
	jobsDispatchedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ariadne_orchestrator_jobs_dispatched_total",
		Help: "Validator Jobs dispatched, by validator kind and final phase.",
	}, []string{"kind", "phase"})

	jobDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ariadne_orchestrator_job_duration_seconds",
		Help:    "Wall-clock time from Job creation to result extraction, by validator kind.",
		Buckets: prometheus.ExponentialBuckets(0.5, 2, 10), // 0.5s .. ~256s
	}, []string{"kind"})
)

func init() {
	prometheus.MustRegister(jobsDispatchedTotal, jobDurationSeconds)
}
