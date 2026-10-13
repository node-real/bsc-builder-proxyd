package proxyd

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"
)

func submissionTestRequest(t *testing.T, method string) (*RPCReq, []*types.Transaction) {
	t.Helper()
	txs := []*types.Transaction{
		types.NewTx(&types.LegacyTx{Nonce: 1, Gas: 21000, GasPrice: big.NewInt(1)}),
		types.NewTx(&types.DynamicFeeTx{Nonce: 2, Gas: 21000, GasFeeCap: big.NewInt(2), GasTipCap: big.NewInt(1)}),
	}
	raws := make([]hexutil.Bytes, len(txs))
	for i, tx := range txs {
		raw, err := tx.MarshalBinary()
		require.NoError(t, err)
		raws[i] = raw
	}
	var params any = raws[:1]
	if method == "eth_sendBundle" {
		params = []any{map[string]any{"txs": raws}}
	}
	b, err := json.Marshal(params)
	require.NoError(t, err)
	return &RPCReq{JSONRPC: "2.0", Method: method, Params: b, ID: json.RawMessage(`1`)}, txs
}

func TestSubmissionHash(t *testing.T) {
	req, txs := submissionTestRequest(t, "eth_sendRawTransaction")
	hash, err := submissionHash(req)
	require.NoError(t, err)
	require.Equal(t, txs[0].Hash(), hash)

	req, txs = submissionTestRequest(t, "eth_sendBundle")
	hash, err = submissionHash(req)
	require.NoError(t, err)
	first, second := txs[0].Hash(), txs[1].Hash()
	require.Equal(t, crypto.Keccak256Hash(first[:], second[:]), hash)

	var params []map[string]any
	require.NoError(t, json.Unmarshal(req.Params, &params))
	params[0]["maxTimestamp"] = 123456
	req.Params, err = json.Marshal(params)
	require.NoError(t, err)
	withExpiry, err := submissionHash(req)
	require.NoError(t, err)
	require.Equal(t, hash, withExpiry)
	params[0]["txs"] = []any{params[0]["txs"].([]any)[1], params[0]["txs"].([]any)[0]}
	req.Params, err = json.Marshal(params)
	require.NoError(t, err)
	reversed, err := submissionHash(req)
	require.NoError(t, err)
	require.NotEqual(t, hash, reversed)
}

func TestMulticallDelay(t *testing.T) {
	for _, method := range []string{"eth_sendRawTransaction"} {
		t.Run(method, func(t *testing.T) {
			req, _ := submissionTestRequest(t, method)
			hash, err := submissionHash(req)
			require.NoError(t, err)
			remainder := int(new(big.Int).Mod(hash.Big(), big.NewInt(10000)).Int64())
			bg := &BackendGroup{stagedForwarding: StagedForwardingConfig{Enabled: true, Threshold: remainder, Delay: TOMLDuration(time.Second)}}
			require.Zero(t, bg.multicallDelay([]*RPCReq{req}), "equal to the threshold must not match")
			bg.stagedForwarding.Threshold++
			require.Equal(t, time.Second, bg.multicallDelay([]*RPCReq{req}))
			bg.stagedForwarding.Threshold = 0
			require.Zero(t, bg.multicallDelay([]*RPCReq{req}))
			bg.stagedForwarding.Threshold = 10000
			bg.stagedForwarding.Enabled = false
			require.Zero(t, bg.multicallDelay([]*RPCReq{req}))
			bg.stagedForwarding.Enabled = true
			req.Params = json.RawMessage(`["0xdeadbeef"]`)
			require.Zero(t, bg.multicallDelay([]*RPCReq{req}))
		})
	}
}

func TestStagedForwardingConfig(t *testing.T) {
	var config Config
	_, err := toml.Decode(`
[backend_groups.builders]
backends = ["first", "second"]
routing_strategy = "multicall"
[backend_groups.builders.staged_forwarding]
enabled = true
threshold = 5000
delay = "100ms"
immediate_backends = ["first"]
`, &config)
	require.NoError(t, err)
	base := *config.BackendGroups["builders"]
	require.NoError(t, base.validateStagedForwarding())
	require.Equal(t, TOMLDuration(100*time.Millisecond), base.StagedForwarding.Delay)
	for _, tc := range []struct {
		name string
		edit func(*BackendGroupConfig)
	}{
		{"strategy", func(c *BackendGroupConfig) { c.RoutingStrategy = FallbackRoutingStrategy }},
		{"consensus", func(c *BackendGroupConfig) { c.ConsensusAware = true }},
		{"negative threshold", func(c *BackendGroupConfig) { c.StagedForwarding.Threshold = -1 }},
		{"large threshold", func(c *BackendGroupConfig) { c.StagedForwarding.Threshold = 10001 }},
		{"zero delay", func(c *BackendGroupConfig) { c.StagedForwarding.Delay = 0 }},
		{"negative delay", func(c *BackendGroupConfig) { c.StagedForwarding.Delay = -1 }},
		{"empty builders", func(c *BackendGroupConfig) { c.StagedForwarding.ImmediateBackends = nil }},
		{"unknown builder", func(c *BackendGroupConfig) { c.StagedForwarding.ImmediateBackends = []string{"unknown"} }},
		{"all builders", func(c *BackendGroupConfig) { c.StagedForwarding.ImmediateBackends = []string{"first", "second"} }},
		{"duplicate builder", func(c *BackendGroupConfig) { c.StagedForwarding.ImmediateBackends = []string{"first", "first"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.edit(&cfg)
			require.Error(t, cfg.validateStagedForwarding())
			cfg.StagedForwarding.Enabled = false
			require.NoError(t, cfg.validateStagedForwarding())
		})
	}
}

func stagedTestBackend(t *testing.T, name string, arrivals chan<- time.Time, fail bool) *Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrivals <- time.Now()
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		var req RPCReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(NewRPCRes(req.ID, "accepted"))
	}))
	t.Cleanup(srv.Close)
	return NewBackend(name, srv.URL, "", semaphore.NewWeighted(10), semaphore.NewWeighted(10), WithMaxRetries(0))
}

func awaitSubmission(t *testing.T, arrivals <-chan time.Time) time.Time {
	t.Helper()
	select {
	case arrival := <-arrivals:
		return arrival
	case <-time.After(3 * time.Second):
		t.Fatal("builder did not receive submission")
		return time.Time{}
	}
}

func TestStagedMulticallForwarding(t *testing.T) {
	const delay = 200 * time.Millisecond
	for _, tc := range []struct {
		name      string
		method    string
		enabled   bool
		threshold int
		fail      bool
		batch     bool
	}{
		{"transaction", "eth_sendRawTransaction", true, 10000, false, false},
		{"bundle", "eth_sendBundle", true, 10000, false, false},
		{"immediate builder failure", "eth_sendRawTransaction", true, 10000, true, false},
		{"batch", "eth_sendRawTransaction", true, 10000, false, true},
		{"disabled", "eth_sendRawTransaction", false, 10000, false, false},
		{"not selected", "eth_sendRawTransaction", true, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second := make(chan time.Time, 2), make(chan time.Time, 2)
			bg := &BackendGroup{
				Name: "builders", routingStrategy: MulticallRoutingStrategy,
				Backends:         []*Backend{stagedTestBackend(t, "first", first, tc.fail), stagedTestBackend(t, "second", second, false)},
				stagedForwarding: StagedForwardingConfig{Enabled: tc.enabled, Threshold: tc.threshold, Delay: TOMLDuration(delay), ImmediateBackends: []string{"first"}},
			}
			req, _ := submissionTestRequest(t, tc.method)
			reqs := []*RPCReq{req}
			if tc.batch {
				bundle, _ := submissionTestRequest(t, "eth_sendBundle")
				bundle.ID = json.RawMessage(`2`)
				reqs = append(reqs, bundle)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := time.Now()
			res, servedBy, err := bg.Forward(ctx, reqs, tc.batch)
			returned := time.Now()
			cancel() // Delayed submissions must survive the client's completion.
			require.NoError(t, err)
			require.Len(t, res, len(reqs))
			for i, response := range res {
				require.Equal(t, reqs[i].ID, response.ID)
				require.Equal(t, "accepted", response.Result)
			}
			delayedArrivals := 0
			for range reqs {
				_ = awaitSubmission(t, first)
				arrival := awaitSubmission(t, second)
				if tc.enabled && tc.threshold > 0 && tc.method == "eth_sendRawTransaction" {
					if arrival.Sub(start) >= delay {
						delayedArrivals++
						if !tc.fail {
							require.True(t, returned.Before(arrival), "successful immediate response must not wait for delayed builders")
						}
					}
					if !tc.fail {
						require.Contains(t, servedBy, "builders/first")
					} else {
						require.Equal(t, "builders/second", servedBy)
					}
				} else {
					require.Less(t, arrival.Sub(start), delay)
				}
			}
			if tc.enabled && tc.threshold > 0 && tc.method == "eth_sendRawTransaction" {
				require.Equal(t, 1, delayedArrivals, "only the raw transaction is delayed, including in a mixed batch")
			}
		})
	}
}
