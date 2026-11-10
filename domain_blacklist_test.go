package proxyd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func domainBlacklistBackend(t *testing.T, result ...string) (string, func() []RPCReq) {
	t.Helper()
	var mu sync.Mutex
	var received []RPCReq
	value := "accepted"
	if len(result) > 0 {
		value = result[0]
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		var reqs []*RPCReq
		batch := bytes.HasPrefix(bytes.TrimSpace(body), []byte("["))
		if batch {
			require.NoError(t, json.Unmarshal(body, &reqs))
		} else {
			var req RPCReq
			require.NoError(t, json.Unmarshal(body, &req))
			reqs = []*RPCReq{&req}
		}
		var responses []*RPCRes
		mu.Lock()
		for _, req := range reqs {
			received = append(received, *req)
			responses = append(responses, NewRPCRes(req.ID, value))
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if batch {
			_ = json.NewEncoder(w).Encode(responses)
		} else {
			_ = json.NewEncoder(w).Encode(responses[0])
		}
	}))
	t.Cleanup(backend.Close)
	return backend.URL, func() []RPCReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]RPCReq(nil), received...)
	}
}

func domainBlacklistServer(t *testing.T, cfg *Config) *Server {
	t.Helper()
	cfg.Server.MaxBodySizeBytes = 1 << 20
	srv, shutdown, err := Start(cfg)
	require.NoError(t, err)
	t.Cleanup(shutdown)
	return srv
}

func domainBlacklistConfig(primaryURL, sideURL string) *Config {
	return &Config{
		Backends: BackendsConfig{
			"primary": {RPCURL: primaryURL},
			"side":    {RPCURL: sideURL, DomainBlacklist: []string{"BLOCKED.example.", "fallback.example"}},
		},
		BackendGroups: BackendGroupsConfig{
			"builders": {Backends: []string{"primary", "side"}, RoutingStrategy: MulticallRoutingStrategy},
			"fallback": {Backends: []string{"side", "primary"}, RoutingStrategy: FallbackRoutingStrategy},
		},
		RPCMethodMappings:       map[string]string{"eth_sendRawTransaction": "builders", "eth_sendBundle": "builders", "eth_chainId": "fallback"},
		DomainRPCMethodMappings: map[string]map[string]string{"fallback.example": {"eth_sendRawTransaction": "fallback"}},
	}
}

func TestDomainBlacklistConfig(t *testing.T) {
	var cfg Config
	_, err := toml.Decode(`[backends.side]
rpc_url = "http://127.0.0.1:1234"
domain_blacklist = ["blocked.example", "other.example"]
`, &cfg)
	require.NoError(t, err)
	require.Equal(t, []string{"blocked.example", "other.example"}, cfg.Backends["side"].DomainBlacklist)
}

func TestDomainBlacklistHTTP(t *testing.T) {
	primaryURL, primaryRequests := domainBlacklistBackend(t, "primary-response")
	sideURL, sideRequests := domainBlacklistBackend(t, "side-response")
	srv := domainBlacklistServer(t, domainBlacklistConfig(primaryURL, sideURL))
	for _, tc := range []struct {
		name, method, origin, host string
		filtered                   bool
	}{
		{"raw transaction", "eth_sendRawTransaction", "blocked.example", "proxy.internal", true},
		{"bundle", "eth_sendBundle", "blocked.example", "proxy.internal", true},
		{"case port and dot", "eth_sendRawTransaction", "Blocked.EXAMPLE.:443", "proxy.internal", true},
		{"forwarded chain", "eth_sendRawTransaction", "blocked.example:443, proxy.internal", "proxy.internal", true},
		{"host fallback", "eth_sendRawTransaction", "", "Blocked.example:8080", true},
		{"forwarded host wins", "eth_sendRawTransaction", "allowed.example", "blocked.example", false},
		{"subdomain is distinct", "eth_sendRawTransaction", "sub.blocked.example", "proxy.internal", false},
		{"fallback group", "eth_sendRawTransaction", "fallback.example", "proxy.internal", true},
		{"query unaffected", "eth_chainId", "blocked.example", "proxy.internal", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := submissionTestRequest(t, tc.method)
			body, err := json.Marshal(req)
			require.NoError(t, err)
			httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
			httpReq.Host = tc.host
			httpReq.Header.Set("X-Forwarded-Host", tc.origin)
			primaryBefore, sideBefore := len(primaryRequests()), len(sideRequests())
			w := httptest.NewRecorder()
			srv.HandleRPC(w, httpReq)
			require.Equal(t, http.StatusOK, w.Code)
			var res RPCRes
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
			require.Equal(t, req.ID, res.ID)
			require.Nil(t, res.Error)
			if tc.method == "eth_chainId" {
				require.Equal(t, "side-response", res.Result)
				require.Len(t, primaryRequests(), primaryBefore)
				require.Len(t, sideRequests(), sideBefore+1)
			} else if tc.filtered {
				require.Equal(t, "primary-response", res.Result)
				require.Len(t, primaryRequests(), primaryBefore+1)
				require.Never(t, func() bool { return len(sideRequests()) != sideBefore }, 25*time.Millisecond, time.Millisecond)
			} else {
				require.Contains(t, []string{"primary-response", "side-response"}, res.Result)
				require.Eventually(t, func() bool { return len(primaryRequests()) == primaryBefore+1 && len(sideRequests()) == sideBefore+1 }, time.Second, time.Millisecond)
			}
		})
	}
}

func TestDomainBlacklistMixedBatch(t *testing.T) {
	primaryURL, primaryRequests := domainBlacklistBackend(t, "primary-response")
	sideURL, sideRequests := domainBlacklistBackend(t, "side-response")
	cfg := domainBlacklistConfig(primaryURL, sideURL)
	cfg.BackendGroups["builders"].Backends = []string{"side", "primary"}
	cfg.RPCMethodMappings["eth_chainId"] = "builders"
	srv := domainBlacklistServer(t, cfg)
	raw, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	bundle, _ := submissionTestRequest(t, "eth_sendBundle")
	bundle.ID = json.RawMessage(`2`)
	query := &RPCReq{JSONRPC: "2.0", ID: json.RawMessage(`3`), Method: "eth_chainId", Params: json.RawMessage(`[]`)}
	body, err := json.Marshal([]*RPCReq{raw, query, bundle})
	require.NoError(t, err)
	httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	httpReq.Header.Set("X-Forwarded-Host", "blocked.example")
	w := httptest.NewRecorder()
	srv.HandleRPC(w, httpReq)
	var responses []*RPCRes
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &responses))
	require.Len(t, responses, 3)
	for i, expected := range []*RPCReq{raw, query, bundle} {
		require.Equal(t, expected.ID, responses[i].ID)
		require.Nil(t, responses[i].Error)
	}
	require.Equal(t, "primary-response", responses[0].Result)
	require.Equal(t, "side-response", responses[1].Result)
	require.Equal(t, "primary-response", responses[2].Result)
	require.Len(t, primaryRequests(), 2)
	require.Len(t, sideRequests(), 1)
	require.Equal(t, "eth_chainId", sideRequests()[0].Method)
}

func TestDomainBlacklistPreservesPrimaryError(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RPCReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		_ = json.NewEncoder(w).Encode(NewRPCErrorRes(req.ID, &RPCErr{Code: -32000, Message: "primary rejected transaction"}))
	}))
	t.Cleanup(primary.Close)
	sideURL, sideRequests := domainBlacklistBackend(t)
	srv := domainBlacklistServer(t, domainBlacklistConfig(primary.URL, sideURL))
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	ctx := context.WithValue(context.Background(), ContextKeyOrigin, "blocked.example")
	responses, _, err := srv.BackendGroups["builders"].Forward(ctx, []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Equal(t, "primary rejected transaction", responses[0].Error.Message)
	require.Empty(t, sideRequests())
}

func TestDomainBlacklistPersistenceAndFallback(t *testing.T) {
	for _, redisDown := range []bool{false, true} {
		for _, unhealthy := range []bool{false, true} {
			t.Run(fmt.Sprintf("redisDown=%t/unhealthy=%t", redisDown, unhealthy), func(t *testing.T) {
				redisServer, _ := pendingTestRedis(t)
				primaryURL, primaryRequests := domainBlacklistBackend(t)
				sideURL, sideRequests := domainBlacklistBackend(t)
				cfg := pendingRestartConfig("redis://"+redisServer.Addr(), primaryURL, sideURL)
				cfg.Backends["second"].DomainBlacklist = []string{"blocked.example"}
				srv := domainBlacklistServer(t, cfg)
				bg := srv.BackendGroups["builders"]
				if redisDown {
					redisServer.Close()
				}
				if unhealthy {
					for _, backend := range bg.Backends {
						backend.maxErrorRateThreshold = 0
					}
				}
				ctx := context.WithValue(context.Background(), ContextKeyOrigin, "blocked.example")
				req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
				responses, _, err := bg.Forward(ctx, []*RPCReq{req}, false)
				require.NoError(t, err)
				require.Nil(t, responses[0].Error)
				require.Equal(t, "accepted", responses[0].Result)
				require.Len(t, primaryRequests(), 1)
				require.Empty(t, sideRequests())
				awaitPendingSendsComplete(t, bg.pendingForwarding)
				if !redisDown {
					require.Empty(t, redisServer.Keys())
				}
			})
		}
	}
}

func TestDomainBlacklistAllBackendsFiltered(t *testing.T) {
	redisServer, _ := pendingTestRedis(t)
	url, received := domainBlacklistBackend(t)
	cfg := pendingRestartConfig("redis://"+redisServer.Addr(), url, url)
	for _, backend := range cfg.Backends {
		backend.DomainBlacklist = []string{"blocked.example"}
	}
	srv := domainBlacklistServer(t, cfg)
	bg := srv.BackendGroups["builders"]
	var commands atomic.Int32
	bg.pendingForwarding.client.(*redis.Client).AddHook(pendingRedisTestHook{process: func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		commands.Add(1)
		return next(ctx, cmd)
	}})
	ctx := context.WithValue(context.Background(), ContextKeyOrigin, "blocked.example")
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	responses, servedBy, err := bg.Forward(ctx, []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Empty(t, servedBy)
	require.Nil(t, responses[0].Error)
	require.Nil(t, responses[0].Result)
	require.Zero(t, commands.Load())
	require.Empty(t, received())
	require.Empty(t, redisServer.Keys())
	// Fallback routing also acknowledges an entirely filtered side request.
	bg.routingStrategy = FallbackRoutingStrategy
	responses, _, err = bg.Forward(ctx, []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Nil(t, responses[0].Error)
	require.Nil(t, responses[0].Result)
}

func TestDomainBlacklistRecovery(t *testing.T) {
	for _, sourceKey := range []string{ContextKeyOrigin, XTxSource} {
		for _, allFiltered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/allFiltered=%t", sourceKey, allFiltered), func(t *testing.T) {
				_, client := pendingTestRedis(t)
				primaryURL, primaryRequests := domainBlacklistBackend(t)
				sideURL, sideRequests := domainBlacklistBackend(t)
				bg := &BackendGroup{Name: "builders", Backends: []*Backend{NewBackend("primary", primaryURL, "", nil, nil), NewBackend("side", sideURL, "", nil, nil)}}
				writer := pendingTestStore(t, client, bg)
				ctx := context.WithValue(context.Background(), sourceKey, "blocked.example")
				req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
				record, err := writer.save(ctx, req)
				require.NoError(t, err)
				bg.Backends[1].Override(WithDomainBlacklist([]string{"blocked.example"}))
				if allFiltered {
					bg.Backends[0].Override(WithDomainBlacklist([]string{"blocked.example"}))
				}
				recovery := pendingTestStore(t, client, bg)
				require.NoError(t, recovery.recoverPending())
				require.Empty(t, sideRequests())
				if allFiltered {
					require.Empty(t, primaryRequests())
				} else {
					require.Len(t, primaryRequests(), 1)
				}
				require.Zero(t, client.ZCard(ctx, recovery.pendingKey()).Val())
				require.Zero(t, client.Exists(ctx, recovery.recordKey(record.id)).Val())
			})
		}
	}
}

func TestDomainBlacklistDelayedBackend(t *testing.T) {
	redisServer, _ := pendingTestRedis(t)
	primaryURL, primaryRequests := domainBlacklistBackend(t)
	sideURL, sideRequests := domainBlacklistBackend(t)
	cfg := pendingRestartConfig("redis://"+redisServer.Addr(), primaryURL, sideURL)
	cfg.Backends["second"].DomainBlacklist = []string{"blocked.example"}
	cfg.BackendGroups[t.Name()] = cfg.BackendGroups["builders"]
	delete(cfg.BackendGroups, "builders")
	cfg.BackendGroups[t.Name()].StagedForwarding.Delay = TOMLDuration(50 * time.Millisecond)
	cfg.RPCMethodMappings["eth_sendRawTransaction"] = t.Name()
	cfg.RPCMethodMappings["eth_sendBundle"] = t.Name()
	srv := domainBlacklistServer(t, cfg)
	bg := srv.BackendGroups[t.Name()]
	ctx := context.WithValue(context.Background(), ContextKeyOrigin, "blocked.example")
	req, _ := submissionTestRequest(t, "eth_sendRawTransaction")
	responses, _, err := bg.Forward(ctx, []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Equal(t, "accepted", responses[0].Result)
	awaitPendingSendsComplete(t, bg.pendingForwarding)
	require.Len(t, primaryRequests(), 1)
	require.Never(t, func() bool { return len(sideRequests()) > 0 }, 100*time.Millisecond, time.Millisecond)
	require.Zero(t, metricValue(t, stagedForwardingDelayedForwardsTotal.WithLabelValues(bg.Name, "second", req.Method, "default")))
	require.Zero(t, metricValue(t, stagedForwardingWaitingRequests.WithLabelValues(bg.Name, req.Method, "default")))
	require.Empty(t, redisServer.Keys())
	// The same side backend still receives allowed domains on the configured timer.
	ctx = context.WithValue(context.Background(), ContextKeyOrigin, "allowed.example")
	_, _, err = bg.Forward(ctx, []*RPCReq{req}, false)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(sideRequests()) == 1 }, time.Second, time.Millisecond)
	awaitPendingSendsComplete(t, bg.pendingForwarding)
	require.Equal(t, float64(1), metricValue(t, stagedForwardingDelayedForwardsTotal.WithLabelValues(bg.Name, "second", req.Method, "default")))
}

func TestDomainBlacklistWebSocket(t *testing.T) {
	for _, allFiltered := range []bool{false, true} {
		t.Run(fmt.Sprintf("allFiltered=%t", allFiltered), func(t *testing.T) {
			var primaryCalls, sideCalls atomic.Int32
			makeWS := func(name string, calls *atomic.Int32) *httptest.Server {
				upgrader := websocket.Upgrader{}
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					for {
						var req RPCReq
						if err := conn.ReadJSON(&req); err != nil {
							return
						}
						calls.Add(1)
						if err := conn.WriteJSON(NewRPCRes(req.ID, name)); err != nil {
							return
						}
					}
				}))
				t.Cleanup(backend.Close)
				return backend
			}
			primaryWS, sideWS := makeWS("primary-response", &primaryCalls), makeWS("side-response", &sideCalls)
			cfg := domainBlacklistConfig("http://127.0.0.1:1", "http://127.0.0.1:2")
			cfg.BackendGroups["builders"].Backends = []string{"side", "primary"}
			cfg.Backends["primary"].WSURL = "ws" + strings.TrimPrefix(primaryWS.URL, "http")
			cfg.Backends["side"].WSURL = "ws" + strings.TrimPrefix(sideWS.URL, "http")
			if allFiltered {
				cfg.Backends["primary"].DomainBlacklist = []string{"blocked.example"}
			}
			cfg.WSBackendGroup = "builders"
			cfg.WSMethodWhitelist = []string{"eth_sendRawTransaction", "eth_sendBundle", "eth_chainId"}
			srv := domainBlacklistServer(t, cfg)
			frontend := httptest.NewServer(http.HandlerFunc(srv.HandleWS))
			t.Cleanup(frontend.Close)
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(frontend.URL, "http"), http.Header{"X-Forwarded-Host": []string{"blocked.example"}})
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
			for _, method := range []string{"eth_sendRawTransaction", "eth_sendBundle", "eth_chainId"} {
				req, _ := submissionTestRequest(t, method)
				require.NoError(t, conn.WriteJSON(req))
				var res RPCRes
				require.NoError(t, conn.ReadJSON(&res))
				require.Nil(t, res.Error)
				if allFiltered && method != "eth_chainId" {
					require.Nil(t, res.Result)
				} else if allFiltered {
					require.Equal(t, "side-response", res.Result)
				} else {
					require.Equal(t, "primary-response", res.Result)
				}
			}
			if allFiltered {
				require.Equal(t, int32(1), sideCalls.Load())
				require.Zero(t, primaryCalls.Load())
			} else {
				require.Equal(t, int32(3), primaryCalls.Load())
				require.Zero(t, sideCalls.Load())
			}
		})
	}
}
