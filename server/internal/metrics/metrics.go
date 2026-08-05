// Package metrics owns the Prometheus registry for the API.
//
// Phase 0 already runs Prometheus and Grafana, so the API is instrumented from
// its first commit rather than having observability retrofitted later. The
// scrape config for this endpoint lives in deploy/monitoring/prometheus.yml.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequests   *prometheus.CounterVec
	HTTPDuration   *prometheus.HistogramVec
	HTTPInFlight   prometheus.Gauge
	BuildInfo      *prometheus.GaugeVec
	SchemaVersion  prometheus.Gauge
	DBPoolAcquired prometheus.GaugeFunc

	// Transfer counters are separate from the duration histogram on purpose:
	// bytes moved and time taken answer different questions, and a 60-second
	// upload in the same histogram as a 5ms metadata lookup makes both
	// unreadable.
	UploadBytes   prometheus.Counter
	DownloadBytes prometheus.Counter

	// GC results, so "is the trash actually being reclaimed" is answerable from
	// a dashboard instead of by reading logs.
	GCBlobsFreed prometheus.Counter
	GCBytesFreed prometheus.Counter

	// Background migration of Phase 1 whole-file blobs into chunks. The counters
	// answer "is the drain making progress" and "how close to done" — a monotonic
	// count that stops climbing is the signal the backlog is cleared.
	MigratedVersions prometheus.Counter
	MigratedBytes    prometheus.Counter
}

// New builds a dedicated registry rather than using the global default. A
// private registry keeps the exposition free of metrics accidentally
// registered by a dependency, and makes the metrics testable in isolation.
func New(version, commit string, poolStats func() float64) *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		Registry: reg,

		HTTPRequests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "privatecloud_http_requests_total",
				Help: "Total HTTP requests by route pattern, method and status class.",
			},
			// Labelled by ROUTE PATTERN, never raw path. A raw-path label would
			// create a new time series per filename and destroy Prometheus.
			[]string{"route", "method", "status"},
		),

		HTTPDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "privatecloud_http_request_duration_seconds",
				Help: "HTTP request duration by route pattern.",
				// Buckets skew fast: most of these are metadata queries. File
				// transfer endpoints will need their own histogram in slice 3,
				// because mixing 5ms lookups and 60s uploads in one histogram
				// makes both unreadable.
				Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
			},
			[]string{"route", "method"},
		),

		HTTPInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "privatecloud_http_requests_in_flight",
			Help: "Number of HTTP requests currently being served.",
		}),

		BuildInfo: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "privatecloud_build_info",
				Help: "Build metadata; value is always 1.",
			},
			[]string{"version", "commit"},
		),

		SchemaVersion: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "privatecloud_schema_version",
			Help: "Applied goose migration version.",
		}),

		UploadBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "privatecloud_upload_bytes_total",
			Help: "Total bytes accepted through file uploads.",
		}),
		DownloadBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "privatecloud_download_bytes_total",
			Help: "Total bytes served through file downloads.",
		}),
		GCBlobsFreed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "privatecloud_gc_blobs_freed_total",
			Help: "Blobs deleted by garbage collection.",
		}),
		GCBytesFreed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "privatecloud_gc_bytes_freed_total",
			Help: "Bytes reclaimed by garbage collection.",
		}),
	}

	m.DBPoolAcquired = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "privatecloud_db_pool_acquired_connections",
			Help: "Connections currently acquired from the pgx pool.",
		},
		poolStats,
	)

	reg.MustRegister(
		m.HTTPRequests,
		m.HTTPDuration,
		m.HTTPInFlight,
		m.BuildInfo,
		m.SchemaVersion,
		m.DBPoolAcquired,
		m.UploadBytes,
		m.DownloadBytes,
		m.GCBlobsFreed,
		m.GCBytesFreed,
		// Go runtime and process metrics: GC pressure, goroutine count, open
		// FDs. The first two are how you spot a leak before it becomes an
		// outage.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m.BuildInfo.WithLabelValues(version, commit).Set(1)

	return m
}
