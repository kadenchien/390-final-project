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
- [ ] Implement basic server: `map[string]int64` counter state with `sync.RWMutex`
- [ ] Write a minimal client that connects and increments a counter
- [ ] Launch five server processes on `:50051`–`:50055`

**Deliverable**: Five servers running locally; client increments "foo" N times and `GetCounter("foo")` returns exactly N.

### Phase 2 — Leader Election (VR-style view change)

- [ ] Add static membership list to each server config (all three addresses)
- [ ] Implement `Ping` RPC handler on every server
- [ ] Implement heartbeat goroutine: ping leader every 200ms, track consecutive misses
- [ ] On 3 missed pings: increment `view_number`, compute new leader, broadcast `ViewChange`
- [ ] Implement `ViewChange` RPC handler: accept view when majority agrees on same `view_number`
- [ ] Leader refuses requests and returns `Unavailable` during view-change window
- [ ] New leader begins serving once view is committed

**Deliverable**: Kill server 1 mid-run; servers 2 and 3 agree on a new leader within ~1 second, visible in logs.

### Phase 3 — Client-Side Interceptor (transparent failover)

- [ ] Install `github.com/grpc-ecosystem/go-grpc-middleware/v2`
- [ ] Implement `requestIDInterceptor`: stamps `clientID` + `requestID` on every outgoing call; reuses `requestID` on retries of the same logical op
- [ ] Implement `LeaderInterceptor` struct with a shared leader tracker (`sync.RWMutex` + current `*grpc.ClientConn` + membership list)
- [ ] Implement `LeaderInterceptor.Unary()`: inspect `redirect_to` on every response; if non-empty, call `rebind()` and loop (up to `maxRedirects`)
- [ ] Wire up interceptor chain with `grpc.WithChainUnaryInterceptor`:
  - outermost: `logging.UnaryClientInterceptor`
  - `requestIDInterceptor`
  - `leaderInterceptor.Unary()`
  - innermost: `retry.UnaryClientInterceptor` with `codes.Unavailable`, max 5, exponential backoff 50ms + 10% jitter
- [ ] Implement server-side: non-leader returns `redirect_to = knownLeaderAddr` in response

**Deliverable**: Client streams 100 increments to "foo"; kill the leader mid-stream; client detects failure, rebinds, all 100 complete with no duplicates in `GetCounter("foo")`.

### Phase 4 — Deduplication Cache

- [ ] Add `ReplyCache` struct to each server with `map[string]CacheEntry`
- [ ] Add cache lookup at the top of `IncrCounter` handler
- [ ] Implement `Replicate` RPC: applies state delta and cache entry atomically on follower
- [ ] Update leader's `IncrCounter`: replicate to all followers before responding to client
- [ ] Add `TransferState` RPC: sends full counter table + reply cache snapshot to a joining replica

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

### Phase 6 — Write-up and Presentation

- [ ] Write-up structured around three questions:
  1. What distributed systems problem does this address? (exactly-once RPC semantics across leader failover)
  2. What are the design decisions and tradeoffs? (VR vs. Raft, client-side redirect vs. proxy, session leases skipped, W=N vs. quorum write)
  3. What do the measurements show? (failover latency, zero dropped requests with dedup, duplicates without)
- [ ] Connect to literature: Liskov VR chapter (view-change protocol), RIFL/RAMCloud (completion record replication — cite and note simplifications)
- [ ] State all simplifying assumptions explicitly and what breaks if they are violated
- [ ] Presentation framing: "This is the failover mechanism inside etcd, stripped to its essence"

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

```bash
# Start five servers
go run ./cmd/server --id=1 --port=50051 --peers=localhost:50052,localhost:50053,localhost:50054,localhost:50055
go run ./cmd/server --id=2 --port=50052 --peers=localhost:50051,localhost:50053,localhost:50054,localhost:50055
go run ./cmd/server --id=3 --port=50053 --peers=localhost:50051,localhost:50052,localhost:50054,localhost:50055
go run ./cmd/server --id=4 --port=50054 --peers=localhost:50051,localhost:50052,localhost:50053,localhost:50055
go run ./cmd/server --id=5 --port=50055 --peers=localhost:50051,localhost:50052,localhost:50053,localhost:50054

# Run the interactive client (connects to initial leader at :50051)
go run ./cmd/client --servers=localhost:50051,localhost:50052,localhost:50053,localhost:50054,localhost:50055

# Run the load generator (100 goroutines, 10 increments each, kill leader after 500 RPCs)
go run ./cmd/loadgen \
  --servers=localhost:50051,localhost:50052,localhost:50053,localhost:50054,localhost:50055 \
  --goroutines=100 \
  --increments=10 \
  --kill-after=500 \
  --kill-signal=SIGKILL \
  --out=results/hard_crash.csv
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
