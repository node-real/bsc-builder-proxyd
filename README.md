# bsc-builder-proxyd

This tool implements `proxyd`, an RPC request router and proxy for BSC Builder. It is based on the [ethereum-optimism/infra proxyd](https://github.com/ethereum-optimism/infra/tree/main/proxyd) service and has been modified to integrate with bsc-builder's mev-guard functionality.

## Original Source

This repository is a fork of the `proxyd` service from the [ethereum-optimism/infra](https://github.com/ethereum-optimism/infra) repository. The original service has been enhanced to support BSC Builder's MEV protection mechanisms.

## Key Features

This proxy does the following things:

1. Whitelists RPC methods.
2. Routes RPC methods to groups of backend services.
3. Automatically retries failed backend requests.
4. Track backend consensus (`latest`, `safe`, `finalized` blocks), peer count and sync state.
5. Re-write requests and responses to enforce consensus.
6. Load balance requests across backend services.
7. Cache immutable responses from backends.
8. Provides metrics to measure request latency, error rates, and the like.
9. **Override eth_call responses** for MEV guard verification (BSC Builder enhancement).


## eth_call Override (BSC Builder Enhancement)

This version of `proxyd` includes a special feature for MEV guard verification that allows overriding `eth_call` responses based on specific address and data combinations.

### Configuration

To configure `eth_call` overrides, add the following section to your configuration file:

```toml
[eth_call_override]
[[eth_call_override.rules]]
address = "0x1234567890123456789012345678901234567890"
value = "0x0"
result = "\"0x0000000000000000000000000000000000000000000000000000000000000001\""

[[eth_call_override.rules]]
address = "0x0987654321098765432109876543210987654321"
value = "0x1000000000000000000"
result = "\"0x0000000000000000000000000000000000000000000000000000000000000000\""
```

### How it works

When an `eth_call` request is received:
1. The proxy checks if the request's `to` address and `value` fields match any configured rules
2. If a match is found, the predefined `result` is returned immediately without forwarding to backends
3. If no match is found, the request is processed normally through the backend services

The matching is case-insensitive for both address and value fields. If no `value` is specified in the request, it defaults to "0x0".


## Staged builder forwarding

Enable staged forwarding on a `multicall` backend group to give selected builders
a configurable head start for a deterministic fraction of submissions:

```toml
[backend_groups.multicall]
backends = ["nodereal", "48club", "blockrazor"]
routing_strategy = "multicall"

[backend_groups.multicall.staged_forwarding]
enabled = true
threshold = 5000
immediate_backends = ["nodereal"]
delay = "100ms"
```

Enabling staged forwarding requires a reachable primary Redis URL in `[redis]`.
Use the same `redis.namespace` and backend group name across process restarts and
replicas that share the same builder configuration:

```toml
[redis]
url = "$REDIS_URL"
namespace = "builder-proxy"
```

For `eth_sendRawTransaction`, the routing hash is the canonical transaction hash.
For `eth_sendBundle`, it is `Keccak256(txHash1 || txHash2 || ...)`, with each hash
encoded as 32 bytes in transaction order. This bundle routing hash is independent
of validity metadata and may differ from the bundle hash returned by a builder.
The full hash is interpreted as an unsigned big-endian integer; requests where
`hash % 10000 < threshold` go immediately to `immediate_backends`, and to the other
eligible builders after `delay`. Nonmatching requests go to all eligible builders
immediately. `threshold = 0` selects none; `threshold = 10000` selects all.

The first successful response is returned as usual, without waiting for delayed
builders. Delayed forwarding continues after the client request completes or is
cancelled. Backend health filtering and retries follow existing multicall behavior;
if no immediate builder is eligible, the request waits for the delayed builders.
Malformed submissions whose hash cannot be calculated use normal immediate
forwarding and upstream validation.

The feature is disabled by default and requires a positive delay and a non-empty
proper subset of the group's backends in `immediate_backends`. With the feature
enabled, JSON-RPC batches are forwarded as individual requests so each submission
gets its own hash and schedule. WebSocket routing is unaffected.

Every transaction or bundle submission in an enabled group is saved to a separate
Redis pending queue **before any builder is contacted**, including submissions
that do not match the sampling threshold. The queue uses the primary Redis client,
independently of the RPC response cache. Redis write failure rejects the request;
it does not fall back to an in-memory queue, even if `fallback_to_memory` is enabled.

Pending records have no TTL and retain the complete RPC request and forwarding
headers. They are deleted only after every configured builder has returned an RPC
response. A JSON-RPC rejection, including an expired bundle or already-known
transaction, counts as completed delivery. Transport failures or skipped unhealthy
builders leave the record pending for another broadcast attempt.

Before opening its RPC listeners, proxyd checks Redis and broadcasts recoverable
records to **all builders immediately**, bypassing sampling, delay and health
filtering. This can make startup wait for the recovery broadcasts. It also checks
for recoverable records every second while running. A process renews a 30-second
owner lease so another replica does not prematurely broadcast its active delayed
submissions. Normal shutdown releases this lease; after abrupt process death,
recovery waits for the lease to expire, then broadcasts without the original delay.

Recovery provides at-least-once delivery while the Redis records remain available:
a crash after sending but before deleting the record can cause duplicate broadcasts.
The recovery guarantee depends on retaining the Redis data and using the same
namespace and enabled backend group configuration on restart. Failed deliveries
remain in Redis until a later attempt receives responses from all builders.

## Usage

Run `make proxyd` to build the binary. No additional dependencies are necessary.

To configure `proxyd` for use, you'll need to create a configuration file to define your proxy backends and routing rules.  Check out [example.config.toml](./example.config.toml) for how to do this alongside a full list of all options with commentary.

Once you have a config file, start the daemon via `proxyd <path-to-config>.toml`.


## Consensus awareness

Starting on v4.0.0, `proxyd` is aware of the consensus state of its backends. This helps minimize chain reorgs experienced by clients.

To enable this behavior, you must set `consensus_aware` value to `true` in the backend group.

When consensus awareness is enabled, `proxyd` will poll the backends for their states and resolve a consensus group based on:
* the common ancestor `latest` block, i.e. if a backend is experiencing a fork, the fork won't be visible to the clients
* the lowest `safe` block
* the lowest `finalized` block
* peer count
* sync state

The backend group then acts as a round-robin load balancer distributing traffic equally across healthy backends in the consensus group, increasing the availability of the proxy.

A backend is considered healthy if it meets the following criteria:
* not banned
* avg 1-min moving window error rate ≤ configurable threshold
* avg 1-min moving window latency ≤ configurable threshold
* peer count ≥ configurable threshold
* `latest` block lag ≤ configurable threshold
* last state update ≤ configurable threshold
* not currently syncing

When a backend is experiencing inconsistent consensus, high error rates or high latency,
the backend will be banned for a configurable amount of time (default 5 minutes)
and won't receive any traffic during this period.


## Tag rewrite

When consensus awareness is enabled, `proxyd` will enforce the consensus state transparently for all the clients.

For example, if a client requests the `eth_getBlockByNumber` method with the `latest` tag,
`proxyd` will rewrite the request to use the resolved latest block from the consensus group
and forward it to the backend.

The following request methods are rewritten:
* `eth_getLogs`
* `eth_newFilter`
* `eth_getBalance`
* `eth_getCode`
* `eth_getTransactionCount`
* `eth_call`
* `eth_getStorageAt`
* `eth_getBlockTransactionCountByNumber`
* `eth_getUncleCountByBlockNumber`
* `eth_getBlockByNumber`
* `eth_getTransactionByBlockNumberAndIndex`
* `eth_getUncleByBlockNumberAndIndex`
* `debug_getRawReceipts`

And `eth_blockNumber` response is overridden with current block consensus.


## Cacheable methods

Cache use Redis and can be enabled for the following immutable methods:

* `eth_chainId`
* `net_version`
* `eth_getBlockTransactionCountByHash`
* `eth_getUncleCountByBlockHash`
* `eth_getBlockByHash`
* `eth_getTransactionByBlockHashAndIndex`
* `eth_getUncleByBlockHashAndIndex`
* `debug_getRawReceipts` (block hash only)


## Metrics

See `metrics.go` for a list of all available metrics.

The metrics port is configurable via the `metrics.port` and `metrics.host` keys in the config.

## Adding Backend SSL Certificates in Docker

The Docker image runs on Alpine Linux. If you get SSL errors when connecting to a backend within Docker, you may need to add additional certificates to Alpine's certificate store. To do this, bind mount the certificate bundle into a file in `/usr/local/share/ca-certificates`. The `entrypoint.sh` script will then update the store with whatever is in the `ca-certificates` directory prior to starting `proxyd`.
