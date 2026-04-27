package server

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/kadenchien/390-final-project/gen/counter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// Server holds the counter state and implements CounterServiceServer.
// Only IncrCounter and GetCounter implemented for now
// Other methods fall through to UnimplementedCounterServiceServer.
type Server struct {
	pb.UnimplementedCounterServiceServer

	mu         sync.RWMutex
	counters   map[string]int64
	id         int      // server's ID
	self       string   // server address
	peers      []string // address of other servers in cluster
	allServers []string // sorted list of all servers (self + peers)
	leaderAddr string   // current known leader address

	viewNumber   int64                     // current view number (accessed atomically)
	viewVotes    map[int64]map[string]bool // votes received per view number
	inViewChange int32                     // 1 while a view change is in progress (accessed atomically)

	cache        *ReplyCache
	dedupEnabled bool
	replyDelay   time.Duration
	peerConns    map[string]*grpc.ClientConn
}

func New(id int, self string, peers []string, dedupEnabled bool, replyDelay time.Duration) *Server {
	all := append([]string{self}, peers...)
	sort.Strings(all)

	conns := make(map[string]*grpc.ClientConn, len(peers))
	for _, peer := range peers {
		conn, err := grpc.NewClient(peer, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err == nil {
			conns[peer] = conn
		}
	}

	return &Server{
		counters:     make(map[string]int64),
		id:           id,
		self:         self,
		peers:        peers,
		allServers:   all,
		leaderAddr:   all[0], // initial leader is the first server in sorted order
		viewVotes:    make(map[int64]map[string]bool),
		cache:        newReplyCache(),
		dedupEnabled: dedupEnabled,
		replyDelay:   replyDelay,
		peerConns:    conns,
	}
}

// Acquires write lock then increments counter by one and returns new value.
// Redirects to the current leader if this server is a follower.
// Returns Unavailable if this server is the leader but a view change is in progress.
func (s *Server) IncrCounter(_ context.Context, req *pb.IncrRequest) (*pb.IncrResponse, error) {
	if req.CounterId == "" {
		return nil, status.Error(codes.InvalidArgument, "counter_id must not be empty")
	}

	if leader := s.currentLeader(); leader != s.self {
		return &pb.IncrResponse{RedirectTo: leader}, nil
	}

	if atomic.LoadInt32(&s.inViewChange) == 1 {
		return nil, status.Error(codes.Unavailable, "view change in progress")
	}

	useDedup := s.dedupEnabled && req.ClientId != ""

	if useDedup {
		if cached, ok := s.cache.get(req.ClientId, req.RequestId); ok {
			return cached, nil
		}
	}

	s.mu.Lock()
	s.counters[req.CounterId]++
	val := s.counters[req.CounterId]
	s.mu.Unlock()

	resp := &pb.IncrResponse{NewValue: val}

	msg := &pb.ReplicateMsg{
		CounterId: req.CounterId,
		NewValue:  val,
	}

	// Store the completion record only when dedup is enabled so Phase 5 can
	// compare exactly-once behavior with and without the cache.
	if useDedup {
		s.cache.set(req.ClientId, req.RequestId, resp)
		msg.ClientId = req.ClientId
		msg.RequestId = req.RequestId
		msg.CachedResponse = resp
	}

	s.replicateToAll(msg)
	if s.replyDelay > 0 {
		time.Sleep(s.replyDelay)
	}
	return resp, nil
}

// Acquires read lock then returns current value (0 if counter doesn't exist yet).
// Redirects to the current leader if this server is a follower.
// Returns Unavailable if this server is the leader but a view change is in progress.
func (s *Server) GetCounter(_ context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	if req.CounterId == "" {
		return nil, status.Error(codes.InvalidArgument, "counter_id must not be empty")
	}

	if leader := s.currentLeader(); leader != s.self {
		return &pb.GetResponse{RedirectTo: leader}, nil
	}

	if atomic.LoadInt32(&s.inViewChange) == 1 {
		return nil, status.Error(codes.Unavailable, "view change in progress")
	}

	s.mu.RLock()
	val := s.counters[req.CounterId]
	s.mu.RUnlock()

	return &pb.GetResponse{Value: val}, nil
}
