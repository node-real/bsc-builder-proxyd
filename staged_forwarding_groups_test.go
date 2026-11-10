package proxyd

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"
)

func signedPolicyTestTransaction(t *testing.T, keyIndex int, txType uint8, protected bool) (*types.Transaction, common.Address) {
	t.Helper()
	key, err := crypto.HexToECDSA(fmt.Sprintf("%064x", keyIndex))
	require.NoError(t, err)
	chainID := big.NewInt(56)
	var tx *types.Transaction
	switch txType {
	case types.LegacyTxType:
		tx = types.NewTx(&types.LegacyTx{Gas: 21000, GasPrice: big.NewInt(1)})
	case types.AccessListTxType:
		tx = types.NewTx(&types.AccessListTx{ChainID: chainID, Gas: 21000, GasPrice: big.NewInt(1)})
	case types.DynamicFeeTxType:
		tx = types.NewTx(&types.DynamicFeeTx{ChainID: chainID, Gas: 21000, GasFeeCap: big.NewInt(2), GasTipCap: big.NewInt(1)})
	default:
		t.Fatalf("unsupported test transaction type %d", txType)
	}
	var signer types.Signer = types.LatestSignerForChainID(chainID)
	if !protected {
		signer = types.HomesteadSigner{}
	}
	signed, err := types.SignTx(tx, signer, key)
	require.NoError(t, err)
	return signed, crypto.PubkeyToAddress(key.PublicKey)
}

func policyTestRequest(t *testing.T, method string, txs ...*types.Transaction) *RPCReq {
	t.Helper()
	raws := make([]hexutil.Bytes, len(txs))
	for i, tx := range txs {
		raw, err := tx.MarshalBinary()
		require.NoError(t, err)
		raws[i] = raw
	}
	var params any = raws
	if method == "eth_sendBundle" {
		params = []any{map[string]any{"txs": raws}}
	}
	b, err := json.Marshal(params)
	require.NoError(t, err)
	return &RPCReq{JSONRPC: "2.0", Method: method, Params: b, ID: json.RawMessage(`1`)}
}

func TestStagedForwardingGroupConfig(t *testing.T) {
	var cfg Config
	_, err := toml.Decode(`
[backend_groups.builders]
backends = ["first", "second", "third"]
routing_strategy = "multicall"
[backend_groups.builders.staged_forwarding]
enabled = true
[backend_groups.builders.staged_forwarding.default]
threshold = 0
[[backend_groups.builders.staged_forwarding.groups]]
name = "priority"
from_addresses = ["0x1111111111111111111111111111111111111111"]
threshold = 10000
delay = "100ms"
immediate_backends = ["second"]
`, &cfg)
	require.NoError(t, err)
	base := *cfg.BackendGroups["builders"]
	require.NoError(t, base.validateStagedForwarding())
	require.Equal(t, 10000, base.StagedForwarding.Groups[0].Threshold)
	require.Equal(t, TOMLDuration(100*time.Millisecond), base.StagedForwarding.Groups[0].Delay)
	require.Equal(t, []string{"second"}, base.StagedForwarding.Groups[0].ImmediateBackends)
	for _, tc := range []struct {
		name string
		edit func(*BackendGroupConfig)
	}{
		{"missing default", func(c *BackendGroupConfig) { c.StagedForwarding.Default = nil }},
		{"mixed legacy config", func(c *BackendGroupConfig) { c.StagedForwarding.Threshold = 5000 }},
		{"empty name", func(c *BackendGroupConfig) { c.StagedForwarding.Groups[0].Name = "" }},
		{"reserved name", func(c *BackendGroupConfig) { c.StagedForwarding.Groups[0].Name = "default" }},
		{"duplicate name", func(c *BackendGroupConfig) {
			c.StagedForwarding.Groups = append(c.StagedForwarding.Groups, c.StagedForwarding.Groups[0])
		}},
		{"empty addresses", func(c *BackendGroupConfig) { c.StagedForwarding.Groups[0].FromAddresses = nil }},
		{"invalid address", func(c *BackendGroupConfig) { c.StagedForwarding.Groups[0].FromAddresses = []string{"0x123"} }},
		{"duplicate address", func(c *BackendGroupConfig) {
			c.StagedForwarding.Groups[0].FromAddresses = []string{"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
		}},
		{"negative threshold", func(c *BackendGroupConfig) { c.StagedForwarding.Groups[0].Threshold = -1 }},
		{"large threshold", func(c *BackendGroupConfig) { c.StagedForwarding.Groups[0].Threshold = 10001 }},
		{"zero delay", func(c *BackendGroupConfig) { c.StagedForwarding.Groups[0].Delay = 0 }},
		{"negative delay", func(c *BackendGroupConfig) { c.StagedForwarding.Groups[0].Delay = -1 }},
		{"unknown builder", func(c *BackendGroupConfig) { c.StagedForwarding.Groups[0].ImmediateBackends = []string{"unknown"} }},
		{"all builders", func(c *BackendGroupConfig) { c.StagedForwarding.Groups[0].ImmediateBackends = c.Backends }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := base
			config.StagedForwarding.Groups = append([]StagedForwardingGroup(nil), base.StagedForwarding.Groups...)
			tc.edit(&config)
			require.Error(t, config.validateStagedForwarding())
		})
	}
	base.StagedForwarding.Groups[0].Threshold = 0
	base.StagedForwarding.Groups[0].Delay = 0
	base.StagedForwarding.Groups[0].ImmediateBackends = nil
	require.NoError(t, base.validateStagedForwarding(), "zero-threshold groups need no delay or priority builders")
}

func TestStagedForwardingFromPolicies(t *testing.T) {
	for _, txType := range []uint8{types.LegacyTxType, types.AccessListTxType, types.DynamicFeeTxType} {
		t.Run(fmt.Sprint(txType), func(t *testing.T) {
			tx, from := signedPolicyTestTransaction(t, 1, txType, true)
			bg := &BackendGroup{stagedForwarding: StagedForwardingConfig{
				Enabled: true, Default: &StagedForwardingPolicy{Threshold: 10000, Delay: TOMLDuration(time.Second), ImmediateBackends: []string{"first"}},
				Groups: []StagedForwardingGroup{{Name: "alice", FromAddresses: []string{strings.ToUpper(from.Hex())},
					StagedForwardingPolicy: StagedForwardingPolicy{Threshold: 10000, Delay: TOMLDuration(200 * time.Millisecond), ImmediateBackends: []string{"second"}}}},
			}}
			req := policyTestRequest(t, "eth_sendRawTransaction", tx)
			plan := bg.multicallPlan([]*RPCReq{req})
			require.Equal(t, "alice", plan.policyGroup)
			require.Equal(t, 200*time.Millisecond, plan.delay)
			require.True(t, plan.isImmediateBackend("second"))
			require.False(t, plan.isImmediateBackend("first"))
			// Matching an address group but missing its threshold must not try default.
			bg.stagedForwarding.Groups[0].Threshold = int(new(big.Int).Mod(tx.Hash().Big(), big.NewInt(10000)).Int64())
			plan = bg.multicallPlan([]*RPCReq{req})
			require.Equal(t, "alice", plan.policyGroup)
			require.Zero(t, plan.delay)
			other, _ := signedPolicyTestTransaction(t, 2, txType, true)
			plan = bg.multicallPlan([]*RPCReq{policyTestRequest(t, "eth_sendRawTransaction", other)})
			require.Equal(t, defaultStagedPolicyGroup, plan.policyGroup)
			require.Equal(t, time.Second, plan.delay)
			require.True(t, plan.isImmediateBackend("first"))
		})
	}
}

func TestStagedForwardingPolicyScopeAndFallback(t *testing.T) {
	tx, from := signedPolicyTestTransaction(t, 1, types.LegacyTxType, false)
	bg := &BackendGroup{stagedForwarding: StagedForwardingConfig{
		Enabled: true, Default: &StagedForwardingPolicy{Threshold: 10000, Delay: TOMLDuration(time.Second)},
		Groups: []StagedForwardingGroup{
			{Name: "immediate", FromAddresses: []string{from.Hex()}, StagedForwardingPolicy: StagedForwardingPolicy{Threshold: 0}},
			{Name: "later", FromAddresses: []string{from.Hex()}, StagedForwardingPolicy: StagedForwardingPolicy{Threshold: 10000, Delay: TOMLDuration(2 * time.Second)}},
		},
	}}
	plan := bg.multicallPlan([]*RPCReq{policyTestRequest(t, "eth_sendRawTransaction", tx)})
	require.Equal(t, "immediate", plan.policyGroup, "unprotected legacy sender recovery and configuration order")
	require.Zero(t, plan.delay)
	unsigned, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	plan = bg.multicallPlan([]*RPCReq{unsigned})
	require.Equal(t, defaultStagedPolicyGroup, plan.policyGroup)
	require.Equal(t, time.Second, plan.delay)
	// Bundles must never enter address-group or default delay policies.
	for _, senderTx := range []*types.Transaction{tx, types.NewTx(&types.LegacyTx{Gas: 21000})} {
		plan = bg.multicallPlan([]*RPCReq{policyTestRequest(t, "eth_sendBundle", senderTx)})
		require.Zero(t, plan.delay)
	}
}

func TestStagedForwardingConcurrentPlans(t *testing.T) {
	alice, aliceFrom := signedPolicyTestTransaction(t, 1, types.DynamicFeeTxType, true)
	bob, _ := signedPolicyTestTransaction(t, 2, types.DynamicFeeTxType, true)
	bg := &BackendGroup{stagedForwarding: StagedForwardingConfig{
		Enabled: true, Default: &StagedForwardingPolicy{Threshold: 10000, Delay: TOMLDuration(time.Second), ImmediateBackends: []string{"first"}},
		Groups: []StagedForwardingGroup{{Name: "alice", FromAddresses: []string{aliceFrom.Hex()},
			StagedForwardingPolicy: StagedForwardingPolicy{Threshold: 10000, Delay: TOMLDuration(2 * time.Second), ImmediateBackends: []string{"second"}}}},
	}}
	aliceReq, bobReq := policyTestRequest(t, "eth_sendRawTransaction", alice), policyTestRequest(t, "eth_sendRawTransaction", bob)
	results := make(chan stagedForwardingPlan, 100)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		for _, req := range []*RPCReq{aliceReq, bobReq} {
			wg.Add(1)
			go func(req *RPCReq) { defer wg.Done(); results <- bg.multicallPlan([]*RPCReq{req}) }(req)
		}
	}
	wg.Wait()
	close(results)
	counts := make(map[string]int)
	for plan := range results {
		counts[plan.policyGroup]++
		if plan.policyGroup == "alice" {
			require.Equal(t, 2*time.Second, plan.delay)
			require.True(t, plan.isImmediateBackend("second"))
		} else {
			require.Equal(t, defaultStagedPolicyGroup, plan.policyGroup)
			require.Equal(t, time.Second, plan.delay)
			require.True(t, plan.isImmediateBackend("first"))
		}
	}
	require.Equal(t, 50, counts["alice"])
	require.Equal(t, 50, counts[defaultStagedPolicyGroup])
}

func TestStagedForwardingGroupForwarding(t *testing.T) {
	_, client := pendingTestRedis(t)
	first, second, third := make(chan time.Time, 2), make(chan time.Time, 2), make(chan time.Time, 2)
	alice, aliceFrom := signedPolicyTestTransaction(t, 1, types.DynamicFeeTxType, true)
	bob, _ := signedPolicyTestTransaction(t, 2, types.LegacyTxType, true)
	bg := &BackendGroup{
		Name: t.Name(), routingStrategy: MulticallRoutingStrategy,
		Backends: []*Backend{stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false), stagedTestBackend(t, "third", third, false)},
		stagedForwarding: StagedForwardingConfig{
			Enabled: true, Default: &StagedForwardingPolicy{Threshold: 10000, Delay: TOMLDuration(250 * time.Millisecond), ImmediateBackends: []string{"first"}},
			Groups: []StagedForwardingGroup{{Name: "alice", FromAddresses: []string{aliceFrom.Hex()},
				StagedForwardingPolicy: StagedForwardingPolicy{Threshold: 10000, Delay: TOMLDuration(150 * time.Millisecond), ImmediateBackends: []string{"second"}}}},
		},
	}
	bg.pendingForwarding = pendingTestStore(t, client, bg)
	for _, tc := range []struct {
		tx       *types.Transaction
		group    string
		priority string
		delay    time.Duration
	}{
		{alice, "alice", "second", 150 * time.Millisecond},
		{bob, defaultStagedPolicyGroup, "first", 250 * time.Millisecond},
	} {
		req := policyTestRequest(t, "eth_sendRawTransaction", tc.tx)
		start := time.Now()
		res, servedBy, err := bg.Forward(context.Background(), []*RPCReq{req}, false)
		returned := time.Now()
		require.NoError(t, err)
		require.Equal(t, "accepted", res[0].Result)
		require.Equal(t, bg.Name+"/"+tc.priority, servedBy)
		for backend, arrivals := range map[string]<-chan time.Time{"first": first, "second": second, "third": third} {
			arrival := awaitSubmission(t, arrivals)
			if backend != tc.priority {
				require.GreaterOrEqual(t, arrival.Sub(start), tc.delay)
				require.True(t, returned.Before(arrival))
				require.Equal(t, float64(1), metricValue(t, stagedForwardingDelayedForwardsTotal.WithLabelValues(bg.Name, backend, req.Method, tc.group)))
			}
		}
		require.Equal(t, float64(1), metricValue(t, stagedForwardingRequestsTotal.WithLabelValues(bg.Name, req.Method, tc.group)))
		require.Zero(t, metricValue(t, stagedForwardingWaitingRequests.WithLabelValues(bg.Name, req.Method, tc.group)))
		require.Eventually(t, func() bool { return client.ZCard(context.Background(), bg.pendingForwarding.pendingKey()).Val() == 0 }, time.Second, 10*time.Millisecond)
	}
}
