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
a configurable head start for a deterministic fraction of `eth_sendRawTransaction`
requests. Policies can be selected by the transaction's signed `from` address:

```toml
[backend_groups.multicall]
backends = ["nodereal", "48club", "blockrazor"]
routing_strategy = "multicall"

[backend_groups.multicall.staged_forwarding]
enabled = true

[backend_groups.multicall.staged_forwarding.default]
threshold = 5000
immediate_backends = ["nodereal"]
delay = "100ms"

[[backend_groups.multicall.staged_forwarding.groups]]
name = "partner_senders"
from_addresses = ["0x1111111111111111111111111111111111111111"]
threshold = 10000
immediate_backends = ["48club"]
delay = "200ms"

[[backend_groups.multicall.staged_forwarding.groups]]
name = "immediate_senders"
from_addresses = ["0x2222222222222222222222222222222222222222"]
threshold = 0
```

Sender addresses are recovered from the raw transaction signature with the
transaction's chain ID. Address matching is case-insensitive and groups are
checked in TOML array order; the first matching group supplies the complete
policy. Each group has independent `threshold`, `delay` and `immediate_backends`.
Only transactions that do not match any address group use `default`. A matching
group's threshold miss means immediate broadcasting, without trying another
group or default. `threshold = 0` disables the delay for that group and needs no
delay or immediate builder configuration.

An explicit `default` policy is required when `groups` are configured. Group names
must be non-empty, unique, and cannot be `default`; malformed or repeated addresses
within a group are rejected at startup. If an address appears in multiple groups,
configuration order decides which policy applies. Transactions whose signatures
cannot yield a sender use `default` and remain subject to upstream validation.
`eth_sendBundle` always uses normal immediate multicall forwarding and does not
enter the address-group or default delay policy.

For compatibility, the previous flat `threshold`, `delay` and `immediate_backends`
under `[backend_groups.<name>.staged_forwarding]` still act as the default raw
transaction policy when no `default` or `groups` are configured. Do not combine
these legacy flat fields with the new nested `default` configuration.

Enabling staged forwarding requires a primary Redis URL in `[redis]`.
Use the same `redis.namespace` and backend group name across process restarts and
replicas that share the same builder configuration:

```toml
[redis]
url = "$REDIS_URL"
namespace = "builder-proxy"
```

For `eth_sendRawTransaction`, the routing hash is the canonical transaction hash.
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

The feature is disabled by default. Each policy with `threshold > 0` requires a
positive delay and a non-empty proper subset of the backend group's builders in
`immediate_backends`. With the feature enabled, JSON-RPC batches are forwarded as
individual requests, so each raw transaction gets its own policy and schedule.
WebSocket routing is unaffected.

Every transaction or bundle submission in an enabled group first attempts to save
to a separate Redis pending queue **before any builder is contacted**, including submissions
that do not match the sampling threshold. The queue uses the primary Redis client,
independently of the RPC response cache. Only a confirmed successful write enables
the raw transaction's staged forwarding policy. On a Redis error or timeout,
the submission is broadcast immediately to **all configured builders**,
bypassing sampling, delay and health filtering. Builder responses determine the RPC
result as usual. Redis operations respect context deadlines (up to 20 milliseconds for
pending operations) in both standalone and cluster modes; fallback begins after
the write attempt fails. This behavior is independent of `fallback_to_memory`.

Each pending record retains the complete RPC request, including raw signed
transaction bytes in `params`, and forwarding headers in an individual Redis
string with a **30-minute TTL from its first successful write**. Startup broadcasts
preserve the remaining TTL; new submissions do not extend older records. Records
are deleted early after every configured builder has returned an RPC response.
A JSON-RPC rejection, including an expired bundle or already-known
transaction, counts as completed delivery. Transport failures or skipped unhealthy
builders leave the record pending for the next Pod startup until its TTL expires.

Keys share the prefix `<redis.namespace>:staged_forwarding:{<hex backend group name>}`:
`:record:<request ID>` stores the full payload, `:pending_index` is a sorted-set
index of record IDs and expiry times. There are no process leases, owner keys or
heartbeats. The index also expires 30 minutes after its last new record and
expired entries are removed on writes, successful completions and startup recovery.
Old `:pending` hash records are
atomically migrated to the new format before recovery, with 30 minutes of retention
from migration, preserving their complete requests and headers.

Before opening its RPC listeners, proxyd checks Redis and broadcasts **all
preexisting, unexpired pending records** in each enabled backend group's namespace
to **all builders immediately**, bypassing sampling, delay and health filtering.
This also intentionally advances existing queued submissions from other live
replicas sharing the same namespace and backend group. The startup
scan uses a fixed cutoff so concurrent new submissions retain their normal policy.
This can make startup wait for the recovery broadcasts.

After startup recovery finishes, the instance never scans or reads queued
transaction payloads during normal operation. It only writes new requests and
deletes records after a complete successful broadcast. Failed deliveries and
failed cleanup remain available for the next startup, bounded by the record TTL.
If Redis is unavailable during startup, proxyd still starts and uses immediate
broadcast whenever persistence fails. It retries only the interrupted startup
recovery pass every second and stops that task once the scan succeeds. Subsequent
successful writes use the configured policy; new submissions received during
startup recovery retain their own forwarding schedule.

Recovery provides at-least-once delivery while the Redis records remain available:
a crash after sending but before deleting the record, or simultaneous Pod startups,
can cause duplicate broadcasts. There is no distributed claim or lease coordination.
The recovery guarantee depends on retaining the Redis data and using the same
namespace and enabled backend group configuration on restart within the record's
30-minute retention window. Failed deliveries remain in Redis until a later attempt
receives responses from all builders or the record expires.
During a Redis outage, submissions that were not persisted have no durable restart
recovery guarantee: immediate broadcasting minimizes the interruption, but a
process crash before sending to every builder can still lose unsent deliveries.

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

Enable the Prometheus endpoint for staged forwarding monitoring:

```toml
[metrics]
enabled = true
host = "0.0.0.0"
port = 9761
```

Scrape `/metrics` on that port. The following metrics distinguish submitted
requests, per-builder forwarding, live delay timers and durable recovery:

| Metric | Labels | Counting rule |
| --- | --- | --- |
| `proxyd_staged_forwarding_redis_fallback_requests_total` | `backend_group`, `method_name` | One per transaction or bundle request broadcast immediately to all configured builders because Redis persistence failed. These requests do not increment delay metrics. |
| `proxyd_staged_forwarding_requests_total` | `backend_group`, `method_name`, `policy_group` | One per raw transaction request matching its selected policy's threshold after Redis persistence succeeds. |
| `proxyd_staged_forwarding_delayed_forwards_total` | `backend_group`, `backend_name`, `method_name`, `policy_group` | One when a delayed builder's timer expires and forwarding starts. Excludes immediate builders, backend retries and recovery broadcasts; does not imply builder acceptance. |
| `proxyd_staged_forwarding_waiting_requests` | `backend_group`, `method_name`, `policy_group` | Requests in this process with at least one builder still waiting for its delay. Each request counts once even if multiple builders are waiting. Decreases when the last timer expires, before network forwarding completes. |
| `proxyd_pending_forwarding_recovery_requests_total` | `backend_group`, `method_name` | One per decoded pending record broadcast during startup recovery. Repeated attempts or simultaneous startups count again. |
| `proxyd_pending_forwarding_records` | `backend_group` | Redis `ZCARD` of the shared pending index after expired entries are removed, including active submissions and failed sends. Sampled on saves, successful completions and startup recovery. |

`policy_group` is the matched group name or `default`. Delay metrics only count
`eth_sendRawTransaction`; `method_name` on the Redis recovery counter still
distinguishes transactions from bundles, with a bundle counting as one request.
For one selected request sent immediately to one builder and after a delay to
two others, the selected-request counter increases by one, the delayed-forward
counter increases by two in total, and the waiting gauge changes from one to zero.
Counters measure submission/forwarding events, not distinct transaction hashes.

Example PromQL for selected requests over the last hour:

```promql
sum by (backend_group, policy_group, method_name) (
  increase(proxyd_staged_forwarding_requests_total[1h])
)
```

For the current number of requests still waiting across replicas, sum
`proxyd_staged_forwarding_waiting_requests`. Replicas sharing a Redis namespace
and backend group report the same Redis queue size: use `max`, not `sum`, for
`proxyd_pending_forwarding_records`, filtering to the same service and namespace:

```promql
max by (backend_group) (proxyd_pending_forwarding_records)
```

There are no periodic runtime queue-size reads. The pending gauge reports the
last successful write/completion/startup sample and can remain stale during idle
periods as records expire. On a Redis sampling error the last successful gauge value
is retained and `proxyd_redis_errors_total{source="PendingForwardingMetrics"}`
is incremented.

## Adding Backend SSL Certificates in Docker

The Docker image runs on Alpine Linux. If you get SSL errors when connecting to a backend within Docker, you may need to add additional certificates to Alpine's certificate store. To do this, bind mount the certificate bundle into a file in `/usr/local/share/ca-certificates`. The `entrypoint.sh` script will then update the store with whatever is in the `ca-certificates` directory prior to starting `proxyd`.
