package proxyd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func pendingTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	server, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(server.Close)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return server, client
}

func pendingTestStore(t *testing.T, client redis.UniversalClient, bg *BackendGroup) *pendingForwardingStore {
	t.Helper()
	store, err := newPendingForwardingStore(client, "test", bg)
	require.NoError(t, err)
	t.Cleanup(store.stop)
	return store
}

func TestPendingForwardingOwnership(t *testing.T) {
	server, client := pendingTestRedis(t)
	first, second := make(chan time.Time, 2), make(chan time.Time, 2)
	bg := &BackendGroup{Name: "builders", Backends: []*Backend{
		stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false),
	}}
	original := pendingTestStore(t, client, bg)
	recovery := pendingTestStore(t, client, bg)
	req, _ := submissionTestRequest(t, "eth_sendBundle")
	record, err := original.save(context.Background(), req)
	require.NoError(t, err)
	require.Zero(t, server.TTL(original.pendingKey()), "payload must not expire with the process lease")
	require.NoError(t, recovery.recoverPending())
	require.Len(t, first, 0, "another live process's tasks must not be recovered")
	require.Len(t, second, 0)
	require.Equal(t, int64(1), client.HLen(context.Background(), original.pendingKey()).Val())

	// Once the old process stops, recovery must immediately attempt all builders.
	original.stop()
	require.NoError(t, recovery.recoverPending())
	awaitSubmission(t, first)
	awaitSubmission(t, second)
	require.Zero(t, client.HLen(context.Background(), original.pendingKey()).Val())
	// Late completion from the old sender must not reinsert a deleted task.
	original.finish(record, false)
	require.Zero(t, client.HLen(context.Background(), original.pendingKey()).Val())
}

func TestPendingForwardingStaleCompletion(t *testing.T) {
	server, client := pendingTestRedis(t)
	bg := &BackendGroup{Name: "builders"}
	original := pendingTestStore(t, client, bg)
	recovery := pendingTestStore(t, client, bg)
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	record, err := original.save(context.Background(), req)
	require.NoError(t, err)
	server.FastForward(pendingOwnerLease)
	require.NoError(t, recovery.renewLease())
	updated := record.submission
	updated.Owner = recovery.owner
	value, err := json.Marshal(updated)
	require.NoError(t, err)
	claimed, err := claimPendingScript.Run(context.Background(), client,
		[]string{recovery.pendingKey(), recovery.ownerKey(original.owner), recovery.ownerKey(recovery.owner)},
		record.id, record.value, string(value)).Int()
	require.NoError(t, err)
	require.Equal(t, 1, claimed)
	original.finish(record, true)
	require.Equal(t, string(value), client.HGet(context.Background(), recovery.pendingKey(), record.id).Val())
	original.finish(record, false)
	require.Equal(t, string(value), client.HGet(context.Background(), recovery.pendingKey(), record.id).Val())
}

func TestPendingForwardingConcurrentRecovery(t *testing.T) {
	_, client := pendingTestRedis(t)
	first, second := make(chan time.Time, 3), make(chan time.Time, 3)
	bg := &BackendGroup{Name: "builders", Backends: []*Backend{
		stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false),
	}}
	original := pendingTestStore(t, client, bg)
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	_, err := original.save(context.Background(), req)
	require.NoError(t, err)
	original.stop()
	stores := []*pendingForwardingStore{pendingTestStore(t, client, bg), pendingTestStore(t, client, bg)}
	var wg sync.WaitGroup
	errors := make(chan error, len(stores))
	for _, store := range stores {
		wg.Add(1)
		go func(store *pendingForwardingStore) {
			defer wg.Done()
			errors <- store.recoverPending()
		}(store)
	}
	wg.Wait()
	for range stores {
		require.NoError(t, <-errors)
	}
	require.Len(t, first, 1, "only one replica may claim the pending record")
	require.Len(t, second, 1)
}

func TestPendingForwardingRecoveryAfterLeaseExpiry(t *testing.T) {
	server, client := pendingTestRedis(t)
	first, second := make(chan time.Time, 2), make(chan time.Time, 2)
	bg := &BackendGroup{Name: "builders", Backends: []*Backend{
		stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false),
	}}
	original := pendingTestStore(t, client, bg)
	req, _ := submissionTestRequest(t, "eth_sendBundle")
	_, err := original.save(context.Background(), req)
	require.NoError(t, err)
	// Stop heartbeat without releasing the lease, as happens on an abrupt exit.
	original.cancel()
	original.wg.Wait()
	recovery := pendingTestStore(t, client, bg)
	require.NoError(t, recovery.recoverPending())
	require.Len(t, first, 0)
	recovery.startRecovery()
	server.SetTTL(original.ownerKey(original.owner), time.Nanosecond)
	server.FastForward(time.Nanosecond)
	awaitSubmission(t, first)
	awaitSubmission(t, second)
	require.Eventually(t, func() bool { return client.HLen(context.Background(), recovery.pendingKey()).Val() == 0 }, time.Second, 10*time.Millisecond)
}

func TestPendingForwardingSaveBeforeBroadcast(t *testing.T) {
	_, client := pendingTestRedis(t)
	first, second := make(chan time.Time, 2), make(chan time.Time, 2)
	bg := &BackendGroup{
		Name: "builders", routingStrategy: MulticallRoutingStrategy,
		Backends:         []*Backend{stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false)},
		stagedForwarding: StagedForwardingConfig{Enabled: true, Threshold: 10000, Delay: TOMLDuration(200 * time.Millisecond), ImmediateBackends: []string{"first"}},
	}
	store := pendingTestStore(t, client, bg)
	bg.pendingForwarding = store
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	ctx := context.Background()
	// Losing the lease prevents caching and must prevent any upstream send.
	require.NoError(t, client.Del(ctx, store.ownerKey(store.owner)).Err())
	_, _, err := bg.Forward(ctx, []*RPCReq{req}, false)
	require.ErrorContains(t, err, "cannot persist builder submission")
	require.Len(t, first, 0)
	require.Len(t, second, 0)
	require.NoError(t, store.renewLease())

	res, _, err := bg.Forward(ctx, []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Equal(t, "accepted", res[0].Result)
	require.Equal(t, int64(1), client.HLen(ctx, store.pendingKey()).Val(), "first response must not delete the delayed task")
	awaitSubmission(t, first)
	awaitSubmission(t, second)
	require.Eventually(t, func() bool { return client.HLen(ctx, store.pendingKey()).Val() == 0 }, time.Second, 10*time.Millisecond)
}

func TestPendingForwardingRedisFailure(t *testing.T) {
	server, client := pendingTestRedis(t)
	first, second := make(chan time.Time, 2), make(chan time.Time, 2)
	bg := &BackendGroup{Name: "builders", routingStrategy: MulticallRoutingStrategy,
		Backends: []*Backend{stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false)}}
	store := pendingTestStore(t, client, bg)
	bg.pendingForwarding = store
	server.Close()
	req, _ := submissionTestRequest(t, "eth_sendBundle")
	_, _, err := bg.Forward(context.Background(), []*RPCReq{req}, false)
	require.ErrorContains(t, err, "cannot persist builder submission")
	require.Len(t, first, 0)
	require.Len(t, second, 0)
	require.NoError(t, server.Restart())
}

func TestPendingForwardingTransportFailure(t *testing.T) {
	_, client := pendingTestRedis(t)
	first := make(chan time.Time, 3)
	var reject atomic.Bool
	reject.Store(true)
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reject.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		// Already-known/expired responses prove delivery; do not replay forever.
		_ = json.NewEncoder(w).Encode(NewRPCErrorRes(json.RawMessage(`1`), ErrInvalidRequest("already known")))
	}))
	t.Cleanup(secondServer.Close)
	secondBackend := stagedTestBackend(t, "unused", make(chan time.Time, 1), false)
	secondBackend.rpcURL = secondServer.URL
	secondBackend.Name = "second"
	bg := &BackendGroup{Name: "builders", Backends: []*Backend{stagedTestBackend(t, "first", first, false), secondBackend}}
	store := pendingTestStore(t, client, bg)
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	record, err := store.save(context.Background(), req)
	require.NoError(t, err)
	store.finish(record, false)
	require.NoError(t, store.recoverPending())
	require.Equal(t, int64(1), client.HLen(context.Background(), store.pendingKey()).Val())
	reject.Store(false)
	require.NoError(t, store.recoverPending())
	require.Zero(t, client.HLen(context.Background(), store.pendingKey()).Val())
	require.Len(t, first, 2, "retry must broadcast to all builders again")
}

func pendingRestartConfig(redisURL, firstURL, secondURL string) *Config {
	return &Config{
		Redis:    RedisConfig{URL: redisURL, Namespace: "restart-test"},
		Backends: BackendsConfig{"first": {RPCURL: firstURL}, "second": {RPCURL: secondURL}},
		BackendGroups: BackendGroupsConfig{"builders": {
			Backends: []string{"first", "second"}, RoutingStrategy: MulticallRoutingStrategy,
			StagedForwarding: StagedForwardingConfig{Enabled: true, Threshold: 10000, Delay: TOMLDuration(30 * time.Second), ImmediateBackends: []string{"first"}},
		}},
		RPCMethodMappings: map[string]string{"eth_sendRawTransaction": "builders", "eth_sendBundle": "builders"},
	}
}

func TestPendingForwardingRestartHelper(t *testing.T) {
	if os.Getenv("PROXYD_PENDING_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	cfg := pendingRestartConfig(os.Getenv("PROXYD_PENDING_REDIS"), os.Getenv("PROXYD_PENDING_FIRST"), os.Getenv("PROXYD_PENDING_SECOND"))
	srv, shutdown, err := Start(cfg)
	require.NoError(t, err)
	req, _ := submissionTestRequest(t, os.Getenv("PROXYD_PENDING_METHOD"))
	ctx := context.WithValue(context.Background(), ContextKeyXForwardedFor, "203.0.113.1")
	ctx = context.WithValue(ctx, ContextKeyOpTxProxyAuth, "test-signature")
	ctx = context.WithValue(ctx, XTxSource, "test-source")
	res, _, err := srv.BackendGroups["builders"].Forward(ctx, []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Equal(t, "accepted", res[0].Result)
	if os.Getenv("PROXYD_PENDING_CRASH") == "1" {
		os.Exit(0) // Exit without running shutdown, like an abrupt process death.
	}
	shutdown()
}

func TestPendingForwardingProcessRestart(t *testing.T) {
	for _, tc := range []struct {
		method string
		crash  bool
	}{
		{"eth_sendRawTransaction", false},
		{"eth_sendBundle", true},
	} {
		t.Run(tc.method, func(t *testing.T) {
			server, client := pendingTestRedis(t)
			type received struct {
				req    RPCReq
				header http.Header
			}
			first, second := make(chan received, 3), make(chan received, 3)
			makeBuilder := func(arrivals chan<- received) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req RPCReq
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						w.WriteHeader(400)
						return
					}
					arrivals <- received{req, r.Header.Clone()}
					_ = json.NewEncoder(w).Encode(NewRPCRes(req.ID, "accepted"))
				}))
			}
			firstBuilder, secondBuilder := makeBuilder(first), makeBuilder(second)
			t.Cleanup(firstBuilder.Close)
			t.Cleanup(secondBuilder.Close)
			redisURL := "redis://" + server.Addr()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPendingForwardingRestartHelper$")
			cmd.Env = append(os.Environ(), "PROXYD_PENDING_HELPER=1", "PROXYD_PENDING_REDIS="+redisURL,
				"PROXYD_PENDING_FIRST="+firstBuilder.URL, "PROXYD_PENDING_SECOND="+secondBuilder.URL,
				"PROXYD_PENDING_METHOD="+tc.method, fmt.Sprintf("PROXYD_PENDING_CRASH=%d", boolToInt(tc.crash)))
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
			require.Len(t, first, 1)
			require.Len(t, second, 0, "delayed builder was not reached before the old process exited")
			original := <-first
			if tc.crash {
				server.FastForward(pendingOwnerLease)
			}
			start := time.Now()
			srv, shutdown, err := Start(pendingRestartConfig(redisURL, firstBuilder.URL, secondBuilder.URL))
			require.NoError(t, err)
			t.Cleanup(shutdown)
			require.Less(t, time.Since(start), 3*time.Second, "recovery must ignore the configured 30s delay")
			require.Len(t, first, 1, "recovery also rebroadcasts to the original builder")
			require.Len(t, second, 1)
			for _, arrival := range []received{<-first, <-second} {
				require.Equal(t, original.req, arrival.req)
				require.Equal(t, "203.0.113.1", arrival.header.Get("X-Forwarded-For"))
				require.Equal(t, "test-signature", arrival.header.Get(DefaultOpTxProxyAuthHeader))
				require.Equal(t, "test-source", arrival.header.Get(XTxSource))
			}
			require.Zero(t, client.HLen(ctx, srv.BackendGroups["builders"].pendingForwarding.pendingKey()).Val())
		})
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestStagedForwardingRequiresRedis(t *testing.T) {
	cfg := pendingRestartConfig("", "http://127.0.0.1:1", "http://127.0.0.1:2")
	_, _, err := Start(cfg)
	require.ErrorContains(t, err, "requires a Redis primary URL")
}
