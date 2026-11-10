package proxyd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
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
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = client.Close() })
	return server, client
}

func pendingTestStore(t *testing.T, client redis.UniversalClient, bg *BackendGroup) *pendingForwardingStore {
	t.Helper()
	store := newPendingForwardingStore(client, "test", bg)
	t.Cleanup(store.stop)
	return store
}

func awaitPendingSendsComplete(t *testing.T, store *pendingForwardingStore) {
	t.Helper()
	require.Eventually(t, func() bool {
		count := 0
		store.active.Range(func(_, _ any) bool { count++; return true })
		return count == 0
	}, pendingRedisTimeout+time.Second, 10*time.Millisecond)
}

func TestPendingForwardingStartupWithoutLease(t *testing.T) {
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
	require.NotContains(t, record.value, `"owner":`, "new payloads must not contain process ownership metadata")
	require.Equal(t, pendingSubmissionTTL, server.TTL(original.recordKey(record.id)))
	for _, key := range server.Keys() {
		require.NotContains(t, key, ":owner:", "saving a submission must not create a process lease")
	}
	require.NoError(t, recovery.recoverPending())
	awaitSubmission(t, first)
	awaitSubmission(t, second)
	require.Zero(t, client.ZCard(context.Background(), original.pendingKey()).Val())
	// Late completion from the old sender must not reinsert a deleted task.
	original.finish(record, false)
	require.Zero(t, client.ZCard(context.Background(), original.pendingKey()).Val())
}

func TestPendingForwardingNoRuntimeRedisReads(t *testing.T) {
	server, _ := pendingTestRedis(t)
	first, second := make(chan time.Time, 1), make(chan time.Time, 1)
	firstBackend, secondBackend := stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false)
	cfg := pendingRestartConfig("redis://"+server.Addr(), firstBackend.rpcURL, secondBackend.rpcURL)
	cfg.BackendGroups["builders"].StagedForwarding.Delay = TOMLDuration(1500 * time.Millisecond)
	srv, shutdown, err := Start(cfg)
	require.NoError(t, err)
	t.Cleanup(shutdown)
	bg := srv.BackendGroups["builders"]
	store := bg.pendingForwarding
	require.True(t, store.startupComplete.Load())
	var reads atomic.Int32
	store.client.(*redis.Client).AddHook(pendingRedisTestHook{process: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		switch cmd.Name() {
		case "get", "hscan", "zscan", "time", "set":
			reads.Add(1)
		}
		return next(ctx, cmd)
	}})
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	res, _, err := bg.Forward(context.Background(), []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Equal(t, "accepted", res[0].Result)
	firstAt := awaitSubmission(t, first)
	// Cover more than one former runtime recovery tick, including an active delay.
	require.Never(t, func() bool { return reads.Load() > 0 }, 2*startupRecoveryRetryInterval, 10*time.Millisecond)
	secondAt := awaitSubmission(t, second)
	require.GreaterOrEqual(t, secondAt.Sub(firstAt), time.Second)
	awaitPendingSendsComplete(t, store)
	for _, key := range server.Keys() {
		require.NotContains(t, key, ":owner:")
	}
}

func TestPendingForwardingRecordTTL(t *testing.T) {
	server, client := pendingTestRedis(t)
	store := pendingTestStore(t, client, &BackendGroup{Name: t.Name()})
	ctx := context.Background()
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	first, err := store.save(ctx, req)
	require.NoError(t, err)
	require.Equal(t, 30*time.Minute, server.TTL(store.recordKey(first.id)))
	var cached pendingSubmission
	require.NoError(t, json.Unmarshal([]byte(client.Get(ctx, store.recordKey(first.id)).Val()), &cached))
	require.Equal(t, req, cached.Request, "Redis must contain the full RPC request, including raw signed transaction bytes in params")

	server.FastForward(15 * time.Minute)
	second, err := store.save(ctx, req)
	require.NoError(t, err)
	require.Equal(t, 15*time.Minute, server.TTL(store.recordKey(first.id)), "new submissions must not extend older records")
	require.Equal(t, 30*time.Minute, server.TTL(store.recordKey(second.id)))
	store.finish(first, false)
	require.Equal(t, 15*time.Minute, server.TTL(store.recordKey(first.id)), "failed delivery must preserve the remaining TTL")
	require.NoError(t, store.recoverPending())
	require.Equal(t, 15*time.Minute, server.TTL(store.recordKey(first.id)), "a startup attempt must not refresh the TTL")

	server.FastForward(15 * time.Minute)
	recovery := pendingTestStore(t, client, &BackendGroup{Name: t.Name()})
	require.NoError(t, recovery.recoverPending())
	require.False(t, server.Exists(store.recordKey(first.id)))
	require.True(t, server.Exists(store.recordKey(second.id)))
	require.Equal(t, int64(1), client.ZCard(ctx, store.pendingKey()).Val(), "the recovery scan must remove expired index entries")
	server.FastForward(15 * time.Minute)
	store.finish(second, false)
	require.False(t, server.Exists(store.recordKey(second.id)), "late completion must not resurrect an expired payload")
	require.False(t, server.Exists(store.pendingKey()), "the index must also expire when no submissions remain")
}

func TestPendingForwardingLegacyMigration(t *testing.T) {
	for _, method := range []string{"eth_sendRawTransaction", "eth_sendBundle"} {
		t.Run(method, func(t *testing.T) {
			server, client := pendingTestRedis(t)
			first, second := make(chan time.Time, 1), make(chan time.Time, 1)
			store := pendingTestStore(t, client, &BackendGroup{Name: t.Name(), Backends: []*Backend{
				stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false),
			}})
			req, _ := submissionTestRequest(t, method)
			value, err := json.Marshal(map[string]any{"owner": "old-process", "request": req, "forwarded_for": "203.0.113.1", "proxy_auth": "signature"})
			require.NoError(t, err)
			ctx := context.Background()
			require.NoError(t, client.HSet(ctx, store.legacyPendingKey(), "legacy-record", value).Err())
			require.NoError(t, store.migrateLegacyPending())
			require.False(t, server.Exists(store.legacyPendingKey()))
			require.Equal(t, 30*time.Minute, server.TTL(store.recordKey("legacy-record")))
			require.Equal(t, string(value), client.Get(ctx, store.recordKey("legacy-record")).Val(), "migration must preserve the entire request and headers")
			require.NoError(t, store.recoverPending())
			awaitSubmission(t, first)
			awaitSubmission(t, second)
			require.False(t, server.Exists(store.recordKey("legacy-record")))
			require.Zero(t, client.ZCard(ctx, store.pendingKey()).Val())
		})
	}
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
	newReplica := func() *pendingForwardingStore {
		replica := &BackendGroup{Name: bg.Name}
		// Separate Pods share Redis and builder URLs, but not Backend statistics.
		for _, backend := range bg.Backends {
			replica.Backends = append(replica.Backends, NewBackend(backend.Name, backend.rpcURL, "", nil, nil, WithMaxRetries(0)))
		}
		return pendingTestStore(t, client, replica)
	}
	stores := []*pendingForwardingStore{newReplica(), newReplica()}
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
	require.GreaterOrEqual(t, len(first), 1)
	require.GreaterOrEqual(t, len(second), 1)
	require.LessOrEqual(t, len(first), len(stores), "simultaneous startups may duplicate delivery but must not lose it")
	require.LessOrEqual(t, len(second), len(stores))
	require.Zero(t, client.ZCard(context.Background(), original.pendingKey()).Val())
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
	var failSave atomic.Bool
	failSave.Store(true)
	client.AddHook(pendingRedisTestHook{process: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		if pendingSaveCommand(cmd) && failSave.Load() {
			return context.DeadlineExceeded
		}
		return next(ctx, cmd)
	}})
	start := time.Now()
	res, _, err := bg.Forward(ctx, []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Equal(t, "accepted", res[0].Result)
	awaitSubmission(t, first)
	require.Less(t, awaitSubmission(t, second).Sub(start), 150*time.Millisecond)
	awaitPendingSendsComplete(t, store)
	failSave.Store(false)

	res, _, err = bg.Forward(ctx, []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Equal(t, "accepted", res[0].Result)
	require.Equal(t, int64(1), client.ZCard(ctx, store.pendingKey()).Val(), "first response must not delete the delayed task")
	awaitSubmission(t, first)
	awaitSubmission(t, second)
	require.Eventually(t, func() bool { return client.ZCard(ctx, store.pendingKey()).Val() == 0 }, time.Second, 10*time.Millisecond)
}

func TestPendingForwardingRedisFailure(t *testing.T) {
	server, client := pendingTestRedis(t)
	first, second := make(chan time.Time, 2), make(chan time.Time, 2)
	bg := &BackendGroup{Name: "builders", routingStrategy: MulticallRoutingStrategy,
		Backends:         []*Backend{stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false)},
		stagedForwarding: StagedForwardingConfig{Enabled: true, Threshold: 10000, Delay: TOMLDuration(time.Hour), ImmediateBackends: []string{"first"}},
	}
	bg.Backends[1].Override(WithMaxErrorRateThreshold(-1))
	store := pendingTestStore(t, client, bg)
	bg.pendingForwarding = store
	server.Close()
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	res, _, err := bg.Forward(context.Background(), []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Equal(t, "accepted", res[0].Result)
	awaitSubmission(t, first)
	awaitSubmission(t, second)
	require.NoError(t, server.Restart())
	awaitPendingSendsComplete(t, store)
}

type pendingRedisTestHook struct {
	process func(context.Context, redis.Cmder, redis.ProcessHook) error
}

func (h pendingRedisTestHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h pendingRedisTestHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h pendingRedisTestHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error { return h.process(ctx, cmd, next) }
}

func pendingScriptKeyCount(cmd redis.Cmder, count int) bool {
	return (cmd.Name() == "eval" || cmd.Name() == "evalsha") && len(cmd.Args()) > 2 && cmd.Args()[2] == count
}

func pendingSaveCommand(cmd redis.Cmder) bool {
	return pendingScriptKeyCount(cmd, 2) && len(cmd.Args()) == 8 && strings.HasSuffix(fmt.Sprint(cmd.Args()[4]), ":pending_index")
}

func TestPendingForwardingSaveTimeoutFallback(t *testing.T) {
	_, client := pendingTestRedis(t)
	first, second := make(chan time.Time, 1), make(chan time.Time, 1)
	bg := &BackendGroup{Name: t.Name(), routingStrategy: MulticallRoutingStrategy,
		Backends:         []*Backend{stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false)},
		stagedForwarding: StagedForwardingConfig{Enabled: true, Threshold: 10000, Delay: TOMLDuration(time.Hour), ImmediateBackends: []string{"first"}},
	}
	bg.pendingForwarding = pendingTestStore(t, client, bg)
	deadlineBudget := make(chan time.Duration, 1)
	client.AddHook(pendingRedisTestHook{process: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		if pendingSaveCommand(cmd) {
			deadline, ok := ctx.Deadline()
			if ok {
				deadlineBudget <- time.Until(deadline)
			}
			<-ctx.Done()
			return ctx.Err()
		}
		return next(ctx, cmd)
	}})
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	start := time.Now()
	res, _, err := bg.Forward(context.Background(), []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Equal(t, "accepted", res[0].Result)
	require.Len(t, deadlineBudget, 1)
	require.LessOrEqual(t, <-deadlineBudget, 20*time.Millisecond, "the Redis write budget must be capped at 20ms without a caller deadline")
	awaitSubmission(t, first)
	require.Less(t, awaitSubmission(t, second).Sub(start), time.Second, "broadcast must continue even after the persistence context times out")
	require.Equal(t, float64(1), metricValue(t, stagedForwardingRedisFallbackRequestsTotal.WithLabelValues(bg.Name, req.Method)))
	awaitPendingSendsComplete(t, bg.pendingForwarding)
}

func TestPendingForwardingUncertainSaveRecovery(t *testing.T) {
	_, client := pendingTestRedis(t)
	first, second := make(chan time.Time, 2), make(chan time.Time, 2)
	bg := &BackendGroup{Name: t.Name(), routingStrategy: MulticallRoutingStrategy,
		Backends:         []*Backend{stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false)},
		stagedForwarding: StagedForwardingConfig{Enabled: true, Threshold: 10000, Delay: TOMLDuration(time.Hour), ImmediateBackends: []string{"first"}},
	}
	store := pendingTestStore(t, client, bg)
	store.startupComplete.Store(true)
	bg.pendingForwarding = store
	var failFinish atomic.Bool
	failFinish.Store(true)
	client.AddHook(pendingRedisTestHook{process: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		if pendingScriptKeyCount(cmd, 2) && !pendingSaveCommand(cmd) && failFinish.Load() {
			return context.DeadlineExceeded
		}
		err := next(ctx, cmd)
		if err == nil && pendingSaveCommand(cmd) {
			// Redis wrote the payload, but the acknowledgement was lost.
			return context.DeadlineExceeded
		}
		return err
	}})
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	res, _, err := bg.Forward(context.Background(), []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Equal(t, "accepted", res[0].Result)
	awaitSubmission(t, first)
	awaitSubmission(t, second)
	awaitPendingSendsComplete(t, store)
	require.Equal(t, int64(1), client.ZCard(context.Background(), store.pendingKey()).Val())
	failFinish.Store(false)
	require.NoError(t, store.recoverPending())
	require.Empty(t, first, "a running instance must not read and rebroadcast cached transactions")
	recovery := pendingTestStore(t, client, bg)
	require.NoError(t, recovery.recoverPending(), "the next startup must recover the retained payload")
	awaitSubmission(t, first)
	awaitSubmission(t, second)
	require.Zero(t, client.ZCard(context.Background(), store.pendingKey()).Val())
}

func TestPendingForwardingStartupRedisUnavailable(t *testing.T) {
	server, client := pendingTestRedis(t)
	first, second := make(chan time.Time, 2), make(chan time.Time, 2)
	firstBackend := stagedTestBackend(t, "first", first, false)
	secondBackend := stagedTestBackend(t, "second", second, false)
	redisURL := "redis://" + server.Addr() + "?max_retries=-1"
	server.Close()
	srv, shutdown, err := Start(pendingRestartConfig(redisURL, firstBackend.rpcURL, secondBackend.rpcURL))
	require.NoError(t, err, "Redis outage must not prevent startup")
	t.Cleanup(shutdown)
	redisRestarted := false
	t.Cleanup(func() {
		if !redisRestarted {
			require.NoError(t, server.Restart())
		}
	})
	bg := srv.BackendGroups["builders"]
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	res, _, err := bg.Forward(context.Background(), []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Equal(t, "accepted", res[0].Result)
	awaitSubmission(t, first)
	awaitSubmission(t, second)
	require.NoError(t, server.Restart())
	redisRestarted = true
	awaitPendingSendsComplete(t, bg.pendingForwarding)
	require.Eventually(t, func() bool {
		return bg.pendingForwarding.startupComplete.Load()
	}, 3*time.Second, 10*time.Millisecond, "only the interrupted startup task may resume after Redis recovers")
	// A successful save must re-enable the configured staged forwarding policy.
	bg.stagedForwarding.Delay = TOMLDuration(200 * time.Millisecond)
	res, _, err = bg.Forward(context.Background(), []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Equal(t, "accepted", res[0].Result)
	firstAt := awaitSubmission(t, first)
	secondAt := awaitSubmission(t, second)
	require.GreaterOrEqual(t, secondAt.Sub(firstAt), 150*time.Millisecond)
	require.Eventually(t, func() bool { return client.ZCard(context.Background(), bg.pendingForwarding.pendingKey()).Val() == 0 }, time.Second, 10*time.Millisecond)
}

func TestPendingForwardingStartupRecoversAllRecords(t *testing.T) {
	_, client := pendingTestRedis(t)
	const count = 12
	first, second := make(chan time.Time, count), make(chan time.Time, count)
	bg := &BackendGroup{Name: "builders", Backends: []*Backend{
		stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false),
	}}
	writers := []*pendingForwardingStore{pendingTestStore(t, client, bg), pendingTestStore(t, client, bg)}
	ctx := context.Background()
	for i := 0; i < count; i++ {
		req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
		req.ID = json.RawMessage(fmt.Sprintf("%d", i+1))
		_, err := writers[i%len(writers)].save(ctx, req)
		require.NoError(t, err)
	}
	cfg := pendingRestartConfig("redis://"+client.Options().Addr, bg.Backends[0].rpcURL, bg.Backends[1].rpcURL)
	cfg.Redis.Namespace = "test"
	srv, shutdown, err := Start(cfg)
	require.NoError(t, err)
	t.Cleanup(shutdown)
	require.Len(t, first, count, "startup must broadcast every queued request written by all replicas")
	require.Len(t, second, count, "startup must not wait for the configured delay")
	store := srv.BackendGroups["builders"].pendingForwarding
	require.True(t, store.startupComplete.Load())
	require.Zero(t, client.ZCard(ctx, store.pendingKey()).Val())
}

func TestPendingForwardingStartupRetryKeepsCutoff(t *testing.T) {
	server, client := pendingTestRedis(t)
	now := time.Now().UTC()
	server.SetTime(now)
	first, second := make(chan time.Time, 2), make(chan time.Time, 2)
	bg := &BackendGroup{Name: t.Name(), Backends: []*Backend{
		stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false),
	}}
	bg.Backends[1].Override(WithMaxErrorRateThreshold(-1))
	original, recovery := pendingTestStore(t, client, bg), pendingTestStore(t, client, bg)
	ctx := context.Background()
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	_, err := original.save(ctx, req)
	require.NoError(t, err)
	var failOnce atomic.Bool
	failOnce.Store(true)
	client.AddHook(pendingRedisTestHook{process: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		if cmd.Name() == "zscan" && failOnce.CompareAndSwap(true, false) {
			return context.DeadlineExceeded
		}
		return next(ctx, cmd)
	}})
	require.ErrorIs(t, recovery.recoverPending(), context.DeadlineExceeded)
	require.False(t, recovery.startupComplete.Load(), "failed startup recovery must remain pending")
	cutoff := recovery.startupCutoff.Load()
	server.SetTime(now.Add(time.Second))
	newer, err := original.save(ctx, req)
	require.NoError(t, err)
	require.NoError(t, recovery.recoverPending())
	require.Equal(t, cutoff, recovery.startupCutoff.Load(), "a retry must not expand the startup recovery scope")
	require.True(t, recovery.startupComplete.Load())
	awaitSubmission(t, first)
	awaitSubmission(t, second)
	require.True(t, server.Exists(original.recordKey(newer.id)), "submissions arriving after startup must retain their normal delay")
	require.NoError(t, recovery.recoverPending())
	require.Empty(t, first, "completed startup recovery must not scan again")
	require.Empty(t, second)
}

func TestPendingForwardingStartupRecoveryAfterRedisOutage(t *testing.T) {
	server, client := pendingTestRedis(t)
	first, second := make(chan time.Time, 1), make(chan time.Time, 1)
	bg := &BackendGroup{Name: "builders", Backends: []*Backend{
		stagedTestBackend(t, "first", first, false), stagedTestBackend(t, "second", second, false),
	}}
	original := pendingTestStore(t, client, bg)
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	_, err := original.save(context.Background(), req)
	require.NoError(t, err)
	cfg := pendingRestartConfig("redis://"+server.Addr()+"?max_retries=-1", bg.Backends[0].rpcURL, bg.Backends[1].rpcURL)
	cfg.Redis.Namespace = "test"
	server.Close()
	srv, shutdown, err := Start(cfg)
	require.NoError(t, err)
	t.Cleanup(shutdown)
	restarted := false
	t.Cleanup(func() {
		if !restarted {
			require.NoError(t, server.Restart())
		}
	})
	store := srv.BackendGroups["builders"].pendingForwarding
	require.False(t, store.startupComplete.Load())
	require.NoError(t, server.Restart())
	restarted = true
	awaitSubmission(t, first)
	awaitSubmission(t, second)
	require.Eventually(t, func() bool {
		return store.startupComplete.Load() && client.ZCard(context.Background(), store.pendingKey()).Val() == 0
	}, time.Second, 10*time.Millisecond)
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
	require.Equal(t, int64(1), client.ZCard(context.Background(), store.pendingKey()).Val())
	reject.Store(false)
	require.NoError(t, store.recoverPending())
	require.Equal(t, int64(1), client.ZCard(context.Background(), store.pendingKey()).Val(), "a completed startup must not retry failed sends during normal operation")
	recovery := pendingTestStore(t, client, bg)
	require.NoError(t, recovery.recoverPending())
	require.Zero(t, client.ZCard(context.Background(), store.pendingKey()).Val())
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
		{"eth_sendRawTransaction", true},
	} {
		t.Run(fmt.Sprintf("%s/crash=%t", tc.method, tc.crash), func(t *testing.T) {
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
			prefix := fmt.Sprintf("%s:staged_forwarding:{%x}", "restart-test", "builders")
			ids, err := client.ZRange(ctx, prefix+":pending_index", 0, -1).Result()
			require.NoError(t, err)
			require.Len(t, ids, 1)
			var stored pendingSubmission
			require.NoError(t, json.Unmarshal([]byte(client.Get(ctx, prefix+":record:"+ids[0]).Val()), &stored))
			require.Equal(t, original.req, *stored.Request, "the old process must persist complete transaction params before it exits")
			require.Positive(t, server.TTL(prefix+":record:"+ids[0]))
			start := time.Now()
			srv, shutdown, err := Start(pendingRestartConfig(redisURL, firstBuilder.URL, secondBuilder.URL))
			require.NoError(t, err)
			t.Cleanup(shutdown)
			require.Less(t, time.Since(start), 3*time.Second, "startup recovery must ignore the configured delay")
			require.Len(t, first, 1, "recovery also rebroadcasts to the original builder")
			require.Len(t, second, 1)
			for _, arrival := range []received{<-first, <-second} {
				require.Equal(t, original.req, arrival.req)
				require.Equal(t, "203.0.113.1", arrival.header.Get("X-Forwarded-For"))
				require.Equal(t, "test-signature", arrival.header.Get(DefaultOpTxProxyAuthHeader))
				require.Equal(t, "test-source", arrival.header.Get(XTxSource))
			}
			require.Zero(t, client.ZCard(ctx, srv.BackendGroups["builders"].pendingForwarding.pendingKey()).Val())
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
