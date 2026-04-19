// Package metrics declares the Prometheus instruments used across the
// orchestrator. All metrics live on the default registry so the
// promhttp.Handler at /metrics picks them up.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// EventsTotal counts provider events received (before dedupe filtering).
	EventsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "code_agent_events_total",
		Help: "Provider events received by source/type.",
	}, []string{"board", "provider", "source", "type"})

	// StageTransitionsTotal counts stage transitions applied.
	StageTransitionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "code_agent_stage_transitions_total",
		Help: "Stage transitions by board/from/to/outcome.",
	}, []string{"board", "from", "to", "outcome"})

	// ActionRunsTotal counts action runner invocations.
	ActionRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "code_agent_action_runs_total",
		Help: "Action runner invocations by action/outcome.",
	}, []string{"action", "outcome"})

	// ActionDurationSeconds observes action latency.
	ActionDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "code_agent_action_duration_seconds",
		Help:    "Action runner latency.",
		Buckets: prometheus.ExponentialBuckets(0.5, 2, 12), // 0.5s .. ~34m
	}, []string{"action"})

	// WorkersActive is a gauge of worker pods per runtime mode.
	WorkersActive = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "code_agent_workers_active",
		Help: "Active workers per runtime mode.",
	}, []string{"mode"})

	// TasksByStage gauges the number of tasks in each stage.
	TasksByStage = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "code_agent_tasks_by_stage",
		Help: "Tasks per stage.",
	}, []string{"board", "stage"})
)
