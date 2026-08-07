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
	//
	// GCReclaimed covers everything a pass frees, labelled by what was freed, so
	// adding a new reclaimable kind does not mean adding a new metric and a new
	// dashboard panel. It exists because only blobs and their bytes were ever
	// published: chunks — which is where most bytes live once CAS is in use —
	// along with manifests, versions, journal entries and derived content were all
	// invisible, so a dashboard could show zero reclaimed while GC was working
	// perfectly well.
	GCBlobsFreed prometheus.Counter
	GCBytesFreed prometheus.Counter
	GCReclaimed  *prometheus.CounterVec
	GCBytes      *prometheus.CounterVec

	// Background migration of Phase 1 whole-file blobs into chunks. The counters
	// answer "is the drain making progress" and "how close to done" — a monotonic
	// count that stops climbing is the signal the backlog is cleared.
	MigratedVersions prometheus.Counter
	MigratedBytes    prometheus.Counter

	// Old versions retired by the retention policy. Watching it climb confirms
	// history is actually being bounded rather than accumulating unnoticed.
	VersionsPruned prometheus.Counter

	// Jobs is the background queue depth by state, polled from the database. A
	// rising 'queued' means the worker is behind or stopped; a nonzero 'failed'
	// means jobs are dead-lettering and deserve a look. This is how the OCR/embed
	// pipeline becomes observable without the worker exposing its own endpoint.
	Jobs *prometheus.GaugeVec

	// The background pipeline's own outcomes. privatecloud_jobs answers "is the
	// queue draining"; these answer "is it doing anything useful", which is a
	// different question — a worker completing every extract job while every
	// document turns out to have no extractable text looks identical on the queue
	// depth alone.
	//
	// Labelled by kind and outcome rather than split into separate counters, so
	// the failure ratio is one query and a new outcome does not need a new metric.
	JobsProcessed *prometheus.CounterVec
	JobDuration   *prometheus.HistogramVec
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
		MigratedVersions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "privatecloud_migrated_versions_total",
			Help: "Whole-file versions rewritten into content-addressed chunks.",
		}),
		MigratedBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "privatecloud_migrated_bytes_total",
			Help: "New chunk bytes written by background migration, after dedup and compression.",
		}),
		VersionsPruned: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "privatecloud_versions_pruned_total",
			Help: "Old file versions removed by the retention policy.",
		}),
		Jobs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "privatecloud_jobs",
			Help: "Background job queue depth by state (queued, running, done, failed).",
		}, []string{"state"}),

		GCReclaimed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "privatecloud_gc_reclaimed_total",
			Help: "Rows or files reclaimed by garbage collection, by kind.",
		}, []string{"kind"}),
		GCBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "privatecloud_gc_reclaimed_bytes_total",
			Help: "Bytes reclaimed by garbage collection, by kind.",
		}, []string{"kind"}),

		JobsProcessed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "privatecloud_jobs_processed_total",
			Help: "Background jobs finished, by kind and outcome (done, failed, panic).",
		}, []string{"kind", "outcome"}),
		JobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "privatecloud_job_duration_seconds",
			Help: "Background job handler duration by kind.",
			// Buckets span seconds to minutes, unlike the HTTP histogram: OCR on a
			// scanned page is tens of seconds and an embed batch can be longer, so
			// the API's millisecond-weighted buckets would put everything in +Inf.
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"kind"}),
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
		m.MigratedVersions,
		m.MigratedBytes,
		m.VersionsPruned,
		m.Jobs,
		m.GCReclaimed,
		m.GCBytes,
		m.JobsProcessed,
		m.JobDuration,
		// Go runtime and process metrics: GC pressure, goroutine count, open
		// FDs. The first two are how you spot a leak before it becomes an
		// outage.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m.BuildInfo.WithLabelValues(version, commit).Set(1)

	return m
}
