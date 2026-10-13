package proxyd

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func metricValue(t *testing.T, metric prometheus.Metric) float64 {
	t.Helper()
	var value dto.Metric
	require.NoError(t, metric.Write(&value))
	if value.Counter != nil {
		return value.GetCounter().GetValue()
	}
	require.NotNil(t, value.Gauge)
	return value.GetGauge().GetValue()
}

func TestStagedForwardingMetrics(t *testing.T) {
	for _, tc := range []struct {
		name      string
		method    string
		enabled   bool
		threshold int
		malformed bool
	}{
		{"transaction", "eth_sendRawTransaction", true, 10000, false},
		{"bundle", "eth_sendBundle", true, 10000, false},
		{"not selected", "eth_sendRawTransaction", true, 0, false},
		{"disabled", "eth_sendRawTransaction", false, 10000, false},
		{"malformed", "eth_sendRawTransaction", true, 10000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, client := pendingTestRedis(t)
			first, second, third := make(chan time.Time, 1), make(chan time.Time, 1), make(chan time.Time, 1)
			bg := &BackendGroup{
				Name: t.Name(), routingStrategy: MulticallRoutingStrategy,
				Backends: []*Backend{
					stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false), stagedTestBackend(t, "third", third, false),
				},
				stagedForwarding: StagedForwardingConfig{Enabled: tc.enabled, Threshold: tc.threshold, Delay: TOMLDuration(500 * time.Millisecond), ImmediateBackends: []string{"first"}},
			}
			if tc.enabled {
				bg.pendingForwarding = pendingTestStore(t, client, bg)
			}
			req, _ := submissionTestRequest(t, tc.method)
			if tc.malformed {
				req.Params = json.RawMessage(`["0xdeadbeef"]`)
			}
			selected := tc.enabled && tc.threshold > 0 && !tc.malformed && tc.method == "eth_sendRawTransaction"
			res, _, err := bg.Forward(context.Background(), []*RPCReq{req}, false)
			require.NoError(t, err)
			require.Equal(t, "accepted", res[0].Result)
			expected := float64(0)
			if selected {
				expected = 1
				require.Equal(t, float64(1), metricValue(t, stagedForwardingWaitingRequests.WithLabelValues(bg.Name, tc.method, defaultStagedPolicyGroup)), "two delayed builders must count as one waiting request")
				for _, backend := range []string{"first", "second", "third"} {
					require.Zero(t, metricValue(t, stagedForwardingDelayedForwardsTotal.WithLabelValues(bg.Name, backend, tc.method, defaultStagedPolicyGroup)), "count actual forwarding starts, not scheduling")
				}
			}
			require.Equal(t, expected, metricValue(t, stagedForwardingRequestsTotal.WithLabelValues(bg.Name, tc.method, defaultStagedPolicyGroup)))
			awaitSubmission(t, first)
			awaitSubmission(t, second)
			awaitSubmission(t, third)
			require.Zero(t, metricValue(t, stagedForwardingWaitingRequests.WithLabelValues(bg.Name, tc.method, defaultStagedPolicyGroup)))
			require.Zero(t, metricValue(t, stagedForwardingDelayedForwardsTotal.WithLabelValues(bg.Name, "first", tc.method, defaultStagedPolicyGroup)))
			for _, backend := range []string{"second", "third"} {
				require.Equal(t, expected, metricValue(t, stagedForwardingDelayedForwardsTotal.WithLabelValues(bg.Name, backend, tc.method, defaultStagedPolicyGroup)))
			}
			require.Zero(t, metricValue(t, pendingForwardingRecoveryRequestsTotal.WithLabelValues(bg.Name, tc.method)))
			if bg.pendingForwarding != nil {
				require.Eventually(t, func() bool { return client.HLen(context.Background(), bg.pendingForwarding.pendingKey()).Val() == 0 }, time.Second, 10*time.Millisecond)
			}
		})
	}
}

func TestStagedForwardingMetricsPersistenceFailure(t *testing.T) {
	_, client := pendingTestRedis(t)
	arrivals := make(chan time.Time, 1)
	bg := &BackendGroup{
		Name: t.Name(), routingStrategy: MulticallRoutingStrategy,
		Backends:         []*Backend{stagedTestBackend(t, "first", arrivals, false)},
		stagedForwarding: StagedForwardingConfig{Enabled: true, Threshold: 10000, Delay: TOMLDuration(time.Second)},
	}
	bg.pendingForwarding = pendingTestStore(t, client, bg)
	require.NoError(t, client.Del(context.Background(), bg.pendingForwarding.ownerKey(bg.pendingForwarding.owner)).Err())
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	_, _, err := bg.Forward(context.Background(), []*RPCReq{req}, false)
	require.ErrorContains(t, err, "cannot persist builder submission")
	require.Len(t, arrivals, 0)
	require.Zero(t, metricValue(t, stagedForwardingRequestsTotal.WithLabelValues(bg.Name, req.Method, defaultStagedPolicyGroup)))
	require.Zero(t, metricValue(t, stagedForwardingWaitingRequests.WithLabelValues(bg.Name, req.Method, defaultStagedPolicyGroup)))
	require.Zero(t, metricValue(t, stagedForwardingDelayedForwardsTotal.WithLabelValues(bg.Name, "first", req.Method, defaultStagedPolicyGroup)))
}

func TestStagedForwardingMetricsNoDelayedTargets(t *testing.T) {
	arrivals := make(chan time.Time, 1)
	bg := &BackendGroup{
		Name: t.Name(), routingStrategy: MulticallRoutingStrategy,
		Backends:         []*Backend{stagedTestBackend(t, "first", arrivals, false)},
		stagedForwarding: StagedForwardingConfig{Enabled: true, Threshold: 10000, Delay: TOMLDuration(time.Second), ImmediateBackends: []string{"first"}},
	}
	skipped := stagedTestBackend(t, "second", make(chan time.Time, 1), false)
	skipped.Override(WithMaxErrorRateThreshold(-1))
	bg.Backends = append(bg.Backends, skipped)
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	_, _, err := bg.Forward(context.Background(), []*RPCReq{req}, false)
	require.NoError(t, err)
	awaitSubmission(t, arrivals)
	require.Equal(t, float64(1), metricValue(t, stagedForwardingRequestsTotal.WithLabelValues(bg.Name, req.Method, defaultStagedPolicyGroup)))
	require.Zero(t, metricValue(t, stagedForwardingWaitingRequests.WithLabelValues(bg.Name, req.Method, defaultStagedPolicyGroup)), "sampling alone does not imply an eligible delayed target")
	require.Zero(t, metricValue(t, stagedForwardingDelayedForwardsTotal.WithLabelValues(bg.Name, "second", req.Method, defaultStagedPolicyGroup)))
}

func TestPendingForwardingRecoveryMetrics(t *testing.T) {
	_, client := pendingTestRedis(t)
	first, second := make(chan time.Time, 1), make(chan time.Time, 1)
	bg := &BackendGroup{
		Name: t.Name(), routingStrategy: MulticallRoutingStrategy,
		Backends:         []*Backend{stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false)},
		stagedForwarding: StagedForwardingConfig{Enabled: true, Threshold: 10000, Delay: TOMLDuration(time.Hour), ImmediateBackends: []string{"first"}},
	}
	original := pendingTestStore(t, client, bg)
	recovery := pendingTestStore(t, client, bg)
	req, _ := submissionTestRequest(t, "eth_sendBundle")
	_, err := original.save(context.Background(), req)
	require.NoError(t, err)
	// Live owners are skipped but their records are part of the Redis backlog.
	require.NoError(t, recovery.recoverPending())
	require.Equal(t, float64(1), metricValue(t, pendingForwardingRecords.WithLabelValues(bg.Name)))
	require.Zero(t, metricValue(t, pendingForwardingRecoveryRequestsTotal.WithLabelValues(bg.Name, req.Method)))
	original.stop()
	require.NoError(t, recovery.recoverPending())
	awaitSubmission(t, first)
	awaitSubmission(t, second)
	require.Equal(t, float64(1), metricValue(t, pendingForwardingRecoveryRequestsTotal.WithLabelValues(bg.Name, req.Method)), "one record broadcast to two builders is one recovery request")
	require.Zero(t, metricValue(t, pendingForwardingRecords.WithLabelValues(bg.Name)))
	require.Zero(t, metricValue(t, stagedForwardingRequestsTotal.WithLabelValues(bg.Name, req.Method, defaultStagedPolicyGroup)))
	require.Zero(t, metricValue(t, stagedForwardingWaitingRequests.WithLabelValues(bg.Name, req.Method, defaultStagedPolicyGroup)))
	for _, backend := range []string{"first", "second"} {
		require.Zero(t, metricValue(t, stagedForwardingDelayedForwardsTotal.WithLabelValues(bg.Name, backend, req.Method, defaultStagedPolicyGroup)), "recovery is not delayed forwarding")
	}
}
