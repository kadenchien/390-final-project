package server

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/kadenchien/390-final-project/gen/counter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// replicate is called by leader on every follower before acking
// applies counter update and reply cache entry so follower is immediately ready to serve as next leader
func (s *Server) Replicate(_ context.Context, msg *pb.ReplicateMsg) (*pb.ReplicateAck, error) {
	s.mu.Lock()
	s.counters[msg.CounterId] = msg.NewValue
	s.mu.Unlock()

	if s.dedupEnabled && msg.ClientId != "" && msg.CachedResponse != nil {
		s.cache.set(msg.ClientId, msg.RequestId, msg.CachedResponse)
	}

	return &pb.ReplicateAck{Ok: true}, nil
}

// transferState called by new or recovering replica to pull full counter table & reply cache from existing replica
func (s *Server) TransferState(_ context.Context, req *pb.TransferReq) (*pb.TransferResp, error) {
	s.mu.RLock()
	counters := make(map[string]int64, len(s.counters))
	for k, v := range s.counters {
		counters[k] = v
	}
	s.mu.RUnlock()

	s.cache.mu.Lock()
	var entries []*pb.CacheEntry
	for key, resp := range s.cache.cache {
		// key format is "clientID:requestID" — split on last colon
		idx := strings.LastIndex(key, ":")
		if idx < 0 {
			continue
		}
		requestID, err := strconv.ParseInt(key[idx+1:], 10, 64)
		if err != nil {
			continue
		}
		entries = append(entries, &pb.CacheEntry{
			ClientId:  key[:idx],
			RequestId: requestID,
			Response:  resp,
		})
	}
	s.cache.mu.Unlock()

	return &pb.TransferResp{
		Counters:   counters,
		Cache:      entries,
		ViewNumber: atomic.LoadInt64(&s.viewNumber),
	}, nil
}

// PullState contacts each peer in turn and applies the first successful TransferState response.
// Called on startup so a joining or recovering server begins with current cluster state.
func (s *Server) PullState() {
	if len(s.peers) == 0 {
		return
	}
	for _, peer := range s.peers {
		if s.tryPullFrom(peer) {
			return
		}
	}
	log.Printf("[%s] PullState: no peers reachable, starting with empty state", s.self)
}

func (s *Server) tryPullFrom(addr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("[%s] PullState: failed to dial %s: %v", s.self, addr, err)
		return false
	}
	defer conn.Close()

	resp, err := pb.NewCounterServiceClient(conn).TransferState(ctx, &pb.TransferReq{RequesterId: s.self})
	if err != nil {
		log.Printf("[%s] PullState: TransferState from %s failed: %v", s.self, addr, err)
		return false
	}

	s.applyTransferResp(resp)
	log.Printf("[%s] PullState: synced from %s (view=%d, counters=%d, cache=%d)",
		s.self, addr, resp.ViewNumber, len(resp.Counters), len(resp.Cache))
	return true
}

// applyTransferResp loads counters, cache, and view number received from a peer.
func (s *Server) applyTransferResp(resp *pb.TransferResp) {
	s.mu.Lock()
	for k, v := range resp.Counters {
		s.counters[k] = v
	}
	s.mu.Unlock()

	if s.dedupEnabled {
		for _, entry := range resp.Cache {
			if entry.ClientId != "" && entry.Response != nil {
				s.cache.set(entry.ClientId, entry.RequestId, entry.Response)
			}
		}
	}

	if resp.ViewNumber > 0 {
		atomic.StoreInt64(&s.viewNumber, resp.ViewNumber)
		s.mu.Lock()
		s.leaderAddr = s.allServers[resp.ViewNumber%int64(len(s.allServers))]
		s.mu.Unlock()
	}
}

// replicateToAll sends Replicate RPC to all peers and waits for all acks before returning.
// If any follower fails, it logs but doesn't prevent the leader from responding to client.
func (s *Server) replicateToAll(msg *pb.ReplicateMsg) {
	var wg sync.WaitGroup
	for _, peer := range s.peers {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				log.Printf("[%s] replicateToAll: failed to dial %s: %v", s.self, addr, err)
				return
			}
			defer conn.Close()
			_, err = pb.NewCounterServiceClient(conn).Replicate(ctx, msg)
			if err != nil {
				log.Printf("[%s] replicateToAll: Replicate to %s failed: %v", s.self, addr, err)
			}
		}(peer)
	}
	wg.Wait()
}
