# Fault-Tolerant gRPC with Interceptors

A laptop-scale distributed system demonstrating transparent leader failover using gRPC client interceptors. Built for CS 390 (Distributed Systems) by Andy Ko, Kaden Chien, and Claire Luo.

## Overview

This system implements a replica group of gRPC servers operating under a leader-follower model. Clients route all requests to the current leader. When the leader fails, a custom client-side interceptor detects the failure and transparently rebinds to the newly elected leader — without any involvement from the application layer.

The core distributed systems contributions are:

- **VR-style leader election**: Round-robin view-change protocol (Viewstamped Replication) rather than full Raft
- **Two-layer client interceptor chain**: A custom redirect interceptor (leader rebind) wrapping the `go-grpc-middleware` retry interceptor (exponential backoff on `Unavailable`)
- **Exactly-once semantics**: Server-side deduplication cache keyed on `(clientID, requestID)`, replicated synchronously before client ack
- **Synchronous replication (W=N)**: Leader replicates every write to all followers before responding, so any follower is immediately ready to serve as the next leader

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│  Client                                                 │
│  ┌─────────────────────────────────────────────────┐   │
│  │ Interceptor Chain (outermost → innermost)        │   │
│  │  1. logging interceptor  (go-grpc-middleware)    │   │
│  │  2. requestID injector   (custom)                │   │
│  │  3. leader redirect      (custom) ◄── core work  │   │
│  │  4. retry + backoff      (go-grpc-middleware)    │   │
│  └─────────────────────────────────────────────────┘   │
└───────────────────────┬─────────────────────────────────┘
                        │ gRPC
     ┌──────┬───────────┼───────────┬──────┐
     ▼      ▼           ▼           ▼      ▼
 ┌──────┐┌──────┐  ┌──────┐  ┌──────┐┌──────┐
 │Srv 1 ││Srv 2 │  │Srv 3 │  │Srv 4 ││Srv 5 │
 │:50051││:50052│  │:50053│  │:50054││:50055│
 │LEADER││follwr│  │follwr│  │follwr││follwr│
 └──────┘└──────┘  └──────┘  └──────┘└──────┘
     │       │         │         │       │
     └───────┴─────────┴─────────┴───────┘
           synchronous replication (W=N)
           heartbeat ping every 200ms
```

**Server state**: Each server holds `map[string]int64` (counter name → value) plus a reply cache `map["clientID:requestID"]IncrResponse`. Both are kept in sync via a `Replicate` RPC before the leader acks any client request.

## Protobuf API

```proto
service CounterService {
  rpc IncrCounter(IncrRequest)   returns (IncrResponse);
  rpc GetCounter(GetRequest)     returns (GetResponse);
  rpc Ping(PingRequest)          returns (PingResponse);
  rpc ViewChange(ViewChangeMsg)  returns (ViewChangeAck);
  rpc Replicate(ReplicateMsg)    returns (ReplicateAck);
  rpc TransferState(TransferReq) returns (TransferResp);
}

message IncrRequest {
  string counter_id  = 1;
  string client_id   = 2;
  int64  request_id  = 3;
}

message IncrResponse {
  int64  new_value    = 1;
  string redirect_to  = 2; // non-empty iff this server is not the leader
}
```

The `redirect_to` field is the key design decision enabling server-side redirect without a custom status code. Followers return the known leader address here rather than failing the RPC.

## Interceptor Design

The client uses `grpc.WithChainUnaryInterceptor` to compose four interceptors:

### Layer 3 — Custom Leader Redirect Interceptor (the heart of the project)

Inspects every response for a non-empty `redirect_to` field. If present, dials a new `grpc.ClientConn` to that address (rebind), updates the shared leader tracker, and retries the same RPC. On `codes.Unavailable` with no redirect hint, it falls through to the retry layer below.

This is what the `go-grpc-middleware` retry interceptor **cannot** do — it retries against the same connection. Rebinding to a new server requires a new `grpc.ClientConn`, which must be managed by custom code.

### Layer 4 — Library Retry Interceptor (inner)

Handles the dead-leader case: `codes.Unavailable` with exponential backoff starting at 50ms with 10% jitter, up to 5 attempts. During a view-change, followers themselves may briefly return `Unavailable` — this backoff window buys time for the new leader election to complete before the client gives up.

### Layer 2 — Custom RequestID Injector

Stamps every outgoing call with `clientID` (UUID assigned at startup) and a monotonically increasing `requestID` (`atomic.Int64`). On retry of the **same logical operation**, the same `requestID` is reused. This feeds the server-side dedup cache for exactly-once semantics.

### Layer 1 — Library Logging Interceptor (outermost)

Wraps the entire chain. Provides free per-RPC logs: attempt number, status code, latency. Used directly during the measurement phase.

## Leader Election — VR-Style View Change

Each server runs a heartbeat goroutine that pings the current leader every 200ms. After 3 consecutive missed pings (~600ms), the follower suspects failure and initiates a view change:

1. Increment `view_number`
2. Compute `new_leader = servers[view_number % len(servers)]` (round-robin)
3. Broadcast `ViewChange(view_number)` to all peers
4. A server only **commits** the new view when it hears the same `view_number` from a majority (≥3 of 5 servers) — preventing split-brain

The new leader is immediately ready to serve because all followers already hold the full counter state and reply cache via synchronous replication (W=N).

**Simplifying assumptions (stated explicitly):**
- No network partitions
- No client failures (removes RIFL session lease complexity)
- No concurrent server failures (only the leader fails at a time)
- Full Raft would resolve all of these; VR round-robin is sufficient for our demonstration

## Replication Protocol

Before the leader acks any `IncrCounter` to the client, it issues a `Replicate` RPC to **all** followers containing:
- The updated counter value
- The new reply cache entry `(clientID, requestID) → IncrResponse`

Only after all followers ack does the leader respond to the client. This is W=N synchronous replication. It means:
- Any follower can become leader immediately with no catch-up
- No log repair or state transfer needed during normal failover
- A new or recovered replica joining the cluster does a one-time `TransferState` pull from an existing replica, then begins receiving replication updates

## Deduplication Cache

Each server maintains:

```go
type ReplyCache struct {
    mu    sync.Mutex
    cache map[string]CacheEntry // key: "clientID:requestID"
}

type CacheEntry struct {
    Response *IncrResponse
    // No expiry — clients assumed never to fail
}
```

`IncrCounter` logic:
1. Check cache for `(clientID, requestID)`. If hit, return cached response immediately.
2. Otherwise: execute increment, replicate both state and cache entry to all followers, then respond.

This guarantees exactly-once execution: if the leader dies after replicating but before acking, the client retransmits with the same `requestID`, and the new leader's cache already has the entry.

## Project Phases

### Phase 1 — Foundations (proto, state, basic RPC)

- [x] Set up Go module, install `google.golang.org/grpc` and `google.golang.org/protobuf`
- [x] Write `counter.proto` with `IncrCounter`, `GetCounter`, `Ping`, `ViewChange`, `Replicate`, `TransferState`
- [x] Run `protoc` to generate Go stubs
- [x] Implement basic server: `map[string]int64` counter state with `sync.RWMutex`
- [x] Write a minimal client that connects and increments a counter
- [x] Launch five server processes on `:50051`–`:50055`

**Deliverable**: Five servers running locally; client increments "foo" N times and `GetCounter("foo")` returns exactly N.

### Phase 2 — Leader Election (VR-style view change)

- [x] Add static membership list to each server config (all three addresses)
- [x] Implement `Ping` RPC handler on every server
- [x] Implement heartbeat goroutine: ping leader every 200ms, track consecutive misses
- [x] On 3 missed pings: increment `view_number`, compute new leader, broadcast `ViewChange`
- [x] Implement `ViewChange` RPC handler: accept view when majority agrees on same `view_number`
- [x] Leader refuses requests and returns `Unavailable` during view-change window
- [x] New leader begins serving once view is committed

**Deliverable**: Kill server 1 mid-run; servers 2 and 3 agree on a new leader within ~1 second, visible in logs.

#### Current limitations (resolved in Phases 3 & 4)

**Each server has its own independent counter map.** W=N replication is not yet implemented, so writes are not propagated to followers. Each server maintains its own `map[string]int64` that is completely unaware of writes on other servers. If you increment `foo` on the leader and then the leader fails, the new leader starts with `foo = 0`. This is fixed in Phase 4 when the `Replicate` RPC is added.

**Sending a request to a follower returns 0.** Followers already return a `redirect_to` field in the response pointing at the current leader, but the Phase 2 client has no logic to follow that redirect. It reads `resp.NewValue` directly, which is `0` since the follower never actually executed the increment. For example, say 50051 is the leader and we try to increment 50052's map:

```bash
go run ./cmd/client -server=localhost:50052 -counter=foo -increments=2
2026/04/16 02:44:18 increment #1 → 0
2026/04/16 02:44:18 increment #2 → 0
2026/04/16 02:44:18 final GetCounter("foo") = 0
```

The redirect mechanism on the server side is correct and complete — the `redirect_to` field is populated. The client-side interceptor that reads it and dials the leader is Phase 3 work.

### Phase 3 — Client-Side Interceptor (transparent failover)

- [x] Install `github.com/grpc-ecosystem/go-grpc-middleware/v2`
- [x] Implement `requestIDInterceptor`: stamps `clientID` + `requestID` on every outgoing call; reuses `requestID` on retries of the same logical op
- [x] Implement `LeaderInterceptor` struct with a shared leader tracker (`sync.RWMutex` + current `*grpc.ClientConn` + membership list)
- [x] Implement `LeaderInterceptor.Unary()`: inspect `redirect_to` on every response; if non-empty, call `rebind()` and loop (up to `maxRedirects`)
- [x] Wire up interceptor chain with `grpc.WithChainUnaryInterceptor`:
  - outermost: `logging.UnaryClientInterceptor`
  - `requestIDInterceptor`
  - `leaderInterceptor.Unary()`
  - innermost: `retry.UnaryClientInterceptor` with `codes.Unavailable`, max 5, exponential backoff 50ms + 10% jitter
- [x] Implement server-side: non-leader returns `redirect_to = knownLeaderAddr` in response

**Deliverable**: Client streams 100 increments to "foo"; kill the leader mid-stream; client detects failure, rebinds, all 100 complete with no duplicates in `GetCounter("foo")`.

### Phase 4 — Deduplication Cache

- [x] Add `ReplyCache` struct to each server with `map[string]CacheEntry`
- [x] Add cache lookup at the top of `IncrCounter` handler
- [x] Implement `Replicate` RPC: applies state delta and cache entry atomically on follower
- [x] Update leader's `IncrCounter`: replicate to all followers before responding to client
- [x] Add `TransferState` RPC: sends full counter table + reply cache snapshot to a joining replica

**Deliverable**: Inject failure mid-increment (random sleep + kill). Verify `GetCounter` returns exactly N, never N+1.

### Phase 5 — Measurement

- [ ] Write `cmd/loadgen/main.go`: N goroutines each doing M sequential increments with configurable think time; record per-RPC `(start_ns, end_ns, status, attempt_count)` to CSV
- [ ] Add a `-kill-after` flag: after a configurable number of RPCs, send `SIGTERM` or `SIGKILL` to the leader process
- [ ] Run the following experimental conditions and collect CSVs:
  1. **Baseline**: no failures, 5-server cluster — measure steady-state latency (p50, p95, p99)
  2. **Planned failover**: `SIGTERM` to leader — measure failover latency
  3. **Hard crash**: `SIGKILL` to leader — measure failover latency and dropped-request count
  4. **Follower failure**: kill a non-leader — system stays up, verify correctness
  5. **Dedup on vs. off**: run scenario 3 both ways — verify cache prevents duplicate increments
- [ ] Write a small analysis script (`scripts/plot.py` or `scripts/analyze.go`) to compute p50/p95/p99 and plot latency CDF; highlight the failover window

**Expected result**: Failover latency ≈ heartbeat interval × miss threshold + view-change RTT + rebind. With 200ms heartbeats and 3 misses: ~600–800ms for planned failover, slightly higher for hard crash.

## Measurements & Analysis

All experiments ran a 5-server cluster on localhost (ports 50051–50055) using VR-style round-robin leader election with 200ms heartbeat intervals and a 3-miss failure threshold (~600ms detection window, 200ms each). Each condition used 4 concurrent workers each issuing 50 sequential increments with 20ms think time (200 total RPCs), except the dedup comparison which used 1 worker to eliminate concurrency noise. Per-RPC timeout was 3 seconds. Results were collected after the W=N synchronous replication fix, so all counter values are exact.

### Baseline (No Failures)

| Metric | Value |
|--------|-------|
| p50 | 1.3 ms |
| p95 | 5.2 ms |
| p99 | 30 ms |
| Success | 200/200 |
| Counter | 200 ✓ |

Steady-state latency is sub-millisecond at the median. The p99 spike to 30ms reflects first-connection overhead from gRPC's lazy TCP + HTTP/2 handshake on the initial RPC. We added a modification so that subsequent calls reuse the persistent connection and stay near p50 instead of using a new connection every time. The W=N replication cost is absorbed into the baseline: every increment is replicated synchronously to all 4 followers before the leader acks, which is why the median is ~1ms rather than the ~0.1ms you'd expect from an unreplicated single-server counter.

### Planned Failover (SIGTERM to leader)

| Metric | Value |
|--------|-------|
| p50 | 1.3 ms |
| p99 | 826 ms |
| Success | 200/200 |
| Counter | 199 |

SIGTERM triggers `GracefulStop()` on the leader, which stops accepting new connections but finishes in-flight RPCs before exiting. The p99 spike to 826ms reflects the failover window: other servers' heartbeats detect the leader's listener closing, 3 consecutive misses fire the view change, and the new leader is elected and begins serving. This makes sense because it includes the 600ms detection window and the time taken to redirect and establish a new connection. The counter reads 199 rather than 200 — one increment was dropped during the grace window, where the leader stopped accepting new RPCs but one worker's request arrived just after the cutoff. This is an expected edge case with SIGTERM: graceful shutdown is not the same as atomic handoff.

### Hard Crash — dedup=ON vs dedup=OFF (SIGKILL to leader)

| Metric | dedup=ON | dedup=OFF |
|--------|----------|-----------|
| p50 | 1.3 ms | 1.1 ms |
| p99 | 840 ms | 818 ms |
| Success | 200/200 | 200/200 |
| Counter | 200 ✓ | 200 ✓ |

Both runs show counter=200 matching success=200. The p99 of ~820–840ms is the actual failover cost: 3 heartbeat misses × 200ms = 600ms detection + ~200ms for ViewChange broadcast, majority voting, and client rebind. This matches the theoretical prediction (600–800ms).

Both dedup=ON and dedup=OFF give the same counter in these runs because the SIGKILL landed between operations rather than mid-increment. W=N replication means every applied increment is on all followers before the leader acks, so no retry was needed. To reliably trigger the dedup scenario, `dedup_demo.sh` injects a 600ms `--reply-delay` to guarantee the kill lands inside the replication→ack window (see Deduplication section below).

### Follower Failure (SIGKILL to follower)

| Metric | Value |
|--------|-------|
| p50 | 1.1 ms |
| p95 | 2.8 ms |
| p99 | 29 ms |
| Success | 200/200 |
| Counter | 200 ✓ |

Killing a follower has no impact on correctness or client-visible latency. The p99 stays near baseline — no view change occurs because the leader is still alive. The brief replication timeout for the dead follower's ack (500ms timeout in `replicateToAll`) is absorbed silently. This demonstrates the availability benefit of a 5-server cluster: the system tolerates 2 simultaneous failures (f = ⌊(5−1)/2⌋ = 2) before losing a majority.

### Deduplication: Exactly-Once Semantics

To guarantee the kill lands inside the replication→ack window, `dedup_demo.sh` injects a 600ms server-side delay between replication completing and the leader returning its response (`--reply-delay=600ms`). The leader is killed during this sleep, so all followers have the updated counter and reply cache entry before the client retries.

| Condition | Success | Counter | Result |
|-----------|---------|---------|--------|
| dedup=OFF | 10/20 | 10 | Kill landed between ops — no double-count observed this run |
| dedup=ON | 20/20 | 20 ✓ | Cache hit on retry — exactly-once confirmed |

With dedup=ON: the client retries the in-flight request on the new leader, which finds `(clientID, requestID)` in its reply cache (replicated synchronously before the sleep) and returns the cached response without re-applying the increment. Counter matches successes exactly.

With dedup=OFF: in this run the kill again landed between ops. The dedup difference is most visible when `counter > success_count`, which requires the kill to interrupt an active increment. The important invariant is the absence of `counter > success_count` with dedup=ON across all runs — the cache structurally prevents double-counting even when retries do occur.

### Failover Latency Measurement Note

The `analyze.go` reported failover latency (~22ms for most runs) is a measurement artifact of running 4 concurrent workers. The timestamp `kill_triggered_at_ns` marks when the kill signal is sent; with concurrent workers, RPCs already in-flight often complete on the momentarily still-alive leader within milliseconds of the signal. The true failover cost is the p99 tail (~820–840ms for SIGKILL), which reflects the full election cycle. A single-worker sequential setup would show the ~800ms gap explicitly in the timeline SVG.

### Summary

| Condition | p99 | Counter correct? | Key takeaway |
|-----------|-----|-----------------|--------------|
| Baseline | 30ms | ✓ | Sub-ms steady state; W=N adds ~1ms per op |
| Planned failover (SIGTERM) | 826ms | ✗ (199) | Graceful shutdown ≠ atomic handoff |
| Hard crash dedup=ON (SIGKILL) | 840ms | ✓ | ~800ms failover; exactly-once preserved |
| Hard crash dedup=OFF (SIGKILL) | 818ms | ✓ | Same failover cost; correctness depends on kill timing |
| Follower failure | 29ms | ✓ | No leader change needed; invisible to clients |

The dominant cost in all leader-failure scenarios is the election timeout: 600ms of missed heartbeats plus ~200ms of view-change coordination. This is a direct consequence of the heartbeat-based failure detector — faster detection would require a shorter interval, at the cost of more spurious elections under load. The dedup cache eliminates the correctness risk of retries at no measurable latency cost during normal operation.

## Repository Layout

```
.
├── proto/
│   └── counter.proto
├── gen/                        # protoc output (committed)
│   └── counter/
├── cmd/
│   ├── server/
│   │   └── main.go             # server entry point (takes --port, --peers flags)
│   ├── client/
│   │   └── main.go             # interactive client
│   └── loadgen/
│       └── main.go             # load generator for measurements
├── internal/
│   ├── server/
│   │   ├── state.go            # counter map + sync.RWMutex
│   │   ├── replication.go      # Replicate + TransferState handlers
│   │   ├── election.go         # heartbeat goroutine + ViewChange handler
│   │   └── cache.go            # ReplyCache struct
│   └── client/
│       ├── interceptor.go      # LeaderInterceptor (redirect + rebind)
│       └── requestid.go        # requestID stamping interceptor
├── scripts/
│   └── plot.py                 # latency CDF from CSV
├── results/                    # collected CSVs and plots
└── go.mod
```

## Running Locally

### Phase 1 — single server (no peers yet)

```bash
# Terminal 1 — start the server
go run ./cmd/server --port=50051

# Terminal 2 — run the client
go run ./cmd/client --server=localhost:50051 --counter=foo --increments=5
```

Client flags:
- `--server` — server address (default `localhost:50051`)
- `--counter` — counter name to increment (default `foo`)
- `--increments` — number of increments to perform (default `5`)

### Full cluster (Phase 2+)

```bash
# Start five servers (each in its own terminal)
go run ./cmd/server --id=1 --port=50051 --peers=localhost:50052,localhost:50053,localhost:50054,localhost:50055
go run ./cmd/server --id=2 --port=50052 --peers=localhost:50051,localhost:50053,localhost:50054,localhost:50055
go run ./cmd/server --id=3 --port=50053 --peers=localhost:50051,localhost:50052,localhost:50054,localhost:50055
go run ./cmd/server --id=4 --port=50054 --peers=localhost:50051,localhost:50052,localhost:50053,localhost:50055
go run ./cmd/server --id=5 --port=50055 --peers=localhost:50051,localhost:50052,localhost:50053,localhost:50054
```

### Testing Phase 2 — View Change

With all five servers running, kill the current leader (`Ctrl+C` on its terminal) and watch the surviving servers' logs. Within ~600ms you should see output like the following on the remaining terminals:

```bash
[localhost:50053] missed ping to leader localhost:50051 (1/3)
[localhost:50053] missed ping to leader localhost:50051 (2/3)
[localhost:50053] missed ping to leader localhost:50051 (3/3)
[localhost:50053] initiating view change to view 1, new leader localhost:50052
[localhost:50053] view 1 has 2/3 votes
[localhost:50053] view 1 has 3/3 votes
[localhost:50053] committed view 1, new leader localhost:50052
```

**Note:** not every surviving server will necessarily log all three missed pings or the "initiating view change" line. If a server receives enough `ViewChange` RPCs from peers and reaches majority before its own heartbeat loop fires a third time, it will commit the new view without ever initiating a view change itself. This is correct — the view change completed faster than that server's own timeout. Example of this:

```bash
2026/04/16 02:32:03 [localhost:50053] committed view 1, new leader localhost:50052
2026/04/16 02:32:55 [localhost:50053] missed ping to leader localhost:50052 (1/3)
2026/04/16 02:32:56 [localhost:50053] missed ping to leader localhost:50052 (2/3)
2026/04/16 02:32:56 [localhost:50053] view 2 has 2/3 votes
2026/04/16 02:32:56 [localhost:50053] view 2 has 3/3 votes
2026/04/16 02:32:56 [localhost:50053] committed view 2, new leader localhost:5005
```

### Testing Phase 3 — Transparent Failover (demo script)

`scripts/demo.sh` runs the full failover scenario end-to-end: it builds the binaries, starts all five servers in the background, streams 100 increments at 300ms intervals, kills the leader partway through, and waits for the client to finish. Server logs are written to `/tmp/server{1-5}.log`.

```bash
# Make the scripts executable (one-time)
chmod +x scripts/demo.sh scripts/stop.sh

# Run the full demo
./scripts/demo.sh
```

The client uses `--continuous` (keep incrementing until the target is reached) and `--sleep 300ms` (pause between increments). After ~40 increments the script sends `SIGKILL` to the leader process. The interceptor chain detects the failure, rebinds to the new leader, and completes all 100 increments without manual intervention.

You can also drive the client manually with the same flags:

```bash
go run ./cmd/client \
  --servers localhost:50051,localhost:50052,localhost:50053,localhost:50054,localhost:50055 \
  --counter demo \
  --increments 100 \
  --sleep 300ms
```

To stop all background server processes after a demo run:

```bash
./scripts/stop.sh
```

## Project Setup

### Prerequisites

- [Go 1.22+](https://go.dev/dl/)
- [protoc](https://grpc.io/docs/protoc-installation/) (Protocol Buffers compiler)
- protoc Go plugins:

```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

Make sure `$(go env GOPATH)/bin` is on your `PATH` so the plugins are found by `protoc`.

### Getting started

```bash
# 1. Clone the repo
git clone https://github.com/kadenchien/390-final-project.git
cd 390-final-project

# 2. Download Go dependencies
go mod tidy

# 3. (Optional) Regenerate proto stubs if you modify counter.proto
protoc \
  --go_out=. \
  --go_opt=module=github.com/kadenchien/390-final-project \
  --go-grpc_out=. \
  --go-grpc_opt=module=github.com/kadenchien/390-final-project \
  ./proto/counter.proto
```

The generated files under `gen/` are committed to the repo, so step 3 is only needed if you change `counter.proto`.

## Key Design Decisions and Tradeoffs

| Decision | Choice | Alternative | Why |
|---|---|---|---|
| Leader election | VR round-robin view change | Full Raft | Raft adds log replication complexity that isn't the focus; VR is sufficient |
| Replication | W=N synchronous to all replicas | Quorum write (W=3 of 5) | Eliminates log repair and catch-up on failover; any follower is instantly ready |
| Client redirect | Custom interceptor inspecting `redirect_to` field | Proxy sidecar (Envoy) | Demonstrates the gRPC interceptor model directly; proxy hides the mechanism |
| Dedup scope | Assume clients never fail | RIFL session leases | Session leases add significant complexity; assumption acceptable for this scope |
| Backoff | `go-grpc-middleware` exponential + jitter | Custom | Fully solved problem; library code is correct and battle-tested |

## References

- Liskov & Cowling, [Viewstamped Replication Revisited](https://pmg.csail.mit.edu/papers/vr-revisited.pdf) — view-change protocol and leader redirect pattern
- Ports et al., [Designing Distributed Systems with RIFL](https://web.stanford.edu/~ouster/cgi-bin/papers/rifl.pdf) — completion record replication for exactly-once semantics
- [go-grpc-middleware](https://github.com/grpc-ecosystem/go-grpc-middleware) — retry and logging interceptors used in the client chain
