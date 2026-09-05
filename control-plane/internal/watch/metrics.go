package watch

import "github.com/prometheus/client_golang/prometheus"

// Metrics defined next to the code that increments them, not in a separate
// package -- see internal/metrics for the shared /metrics HTTP server that
// exposes whatever the default registry (which promauto registers into)
// has collected.
var (
	changeEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ariadne_sensor_change_events_total",
		Help: "ChangeEvents published, by class and operation.",
	}, []string{"change_class", "operation"})

	changeEventsNoiseTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ariadne_sensor_change_events_noise_total",
		Help: "MODIFIED events suppressed as pure resync noise (empty diff).",
	})

	informersSynced = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ariadne_sensor_informers_synced",
		Help: "1 once all watched-resource informer caches have synced, else 0.",
	})

	brokerSubscribers = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ariadne_sensor_broker_subscribers",
		Help: "Currently connected ChangeStream subscribers.",
	})
)

func init() {
	prometheus.MustRegister(changeEventsTotal, changeEventsNoiseTotal, informersSynced, brokerSubscribers)
}
