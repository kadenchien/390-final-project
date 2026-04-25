package server

import (
	"context"
	"log"
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
	for _, resp := range s.cache.cache {
		entries = append(entries, &pb.CacheEntry{
			Response: resp,
		})
	}
	s.cache.mu.Unlock()

	return &pb.TransferResp{
		Counters:   counters,
		Cache:      entries,
		ViewNumber: s.viewNumber,
	}, nil
}

// replicatetoAll sends Replicate RPC to all peers and waits for all acks before returning
// if any follower fails, it logs but doesn't prevent from leader from responding to client
func (s *Server) replicateToAll(msg *pb.ReplicateMsg) {
	for _, peer := range s.peers {
		go func(addr string) {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				log.Printf("[%s] replicatetoAll: failed to dial %s: %v", s.self, addr, err)
				return
			}
			defer conn.Close()
			_, err = pb.NewCounterServiceClient(conn).Replicate(ctx, msg)
			if err != nil {
				log.Printf("[%s] replicatedToAll: Replicate to %s failed: %v", s.self, addr, err)
			}
		}(peer)
	}
}
