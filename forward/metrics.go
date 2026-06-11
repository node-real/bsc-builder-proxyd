package forward

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Client send metrics
	sendTxTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "forward_client_tx_total",
		Help: "Total transactions sent via QUIC forward.",
	}, []string{"status", "private"})

	sendBundleTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "forward_client_bundle_total",
		Help: "Total bundles sent via QUIC forward.",
	}, []string{"status"})

	forwardLatencyHist = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "forward_client_latency_ms",
		Help:    "Latency of QUIC forward sends (ms).",
		Buckets: []float64{1, 5, 10, 25, 50, 100, 200, 500},
	})

	clientConnectionGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "forward_client_connections",
		Help: "Number of active QUIC client connections.",
	})

	workerQueueLengthGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "forward_client_queue_length",
		Help: "Current worker queue length per remote.",
	}, []string{"remote"})

	taskDroppedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "forward_client_task_dropped_total",
		Help: "Tasks dropped due to full worker queue.",
	}, []string{"remote"})

	// Server receive metrics
	receiveTxTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "forward_server_tx_total",
		Help: "Total transactions received via QUIC forward.",
	}, []string{"status", "private", "source"})

	receiveBundleTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "forward_server_bundle_total",
		Help: "Total bundles received via QUIC forward.",
	}, []string{"status", "source"})

	receiveLatencyHist = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "forward_server_latency_ms",
		Help:    "Latency from forward timestamp to local receipt (ms).",
		Buckets: []float64{1, 5, 10, 25, 50, 100, 200, 500, 1000},
	}, []string{"source"})

	serverConnectionGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "forward_server_connections_active",
		Help: "Number of active QUIC server connections.",
	})
)

func privateLabelStr(isPrivate bool) string {
	if isPrivate {
		return "true"
	}
	return "false"
}

func RecordSendTx(isPrivate bool, status string) {
	sendTxTotal.WithLabelValues(status, privateLabelStr(isPrivate)).Inc()
}

func RecordSendBundle(status string) {
	sendBundleTotal.WithLabelValues(status).Inc()
}

func UpdateForwardLatency(latencyMs int64) {
	forwardLatencyHist.Observe(float64(latencyMs))
}

func UpdateClientConnectionCount(count int64) {
	clientConnectionGauge.Set(float64(count))
}

func UpdateWorkerPoolQueueLength(remote string, length int) {
	workerQueueLengthGauge.WithLabelValues(remote).Set(float64(length))
}

func RecordTaskDropped(remote string) {
	taskDroppedTotal.WithLabelValues(remote).Inc()
}

func RecordReceiveTx(isPrivate bool, status string, source string) {
	receiveTxTotal.WithLabelValues(status, privateLabelStr(isPrivate), source).Inc()
}

func RecordReceiveBundle(status string, source string) {
	receiveBundleTotal.WithLabelValues(status, source).Inc()
}

func UpdateReceiveLatency(source string, latencyMs int64) {
	receiveLatencyHist.WithLabelValues(source).Observe(float64(latencyMs))
}

func UpdateServerConnectionCount(count int64) {
	serverConnectionGauge.Set(float64(count))
}
