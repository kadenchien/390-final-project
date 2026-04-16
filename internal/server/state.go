package server

import (
	"context"
	"sort"
	"sync"

	pb "github.com/kadenchien/390-final-project/gen/counter"
	"google.golang.org/grpc/codes"
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

	viewNumber int64                     // current view number (accessed atomically)
	viewVotes  map[int64]map[string]bool // votes received per view number
}

func New(id int, self string, peers []string) *Server {
	all := append([]string{self}, peers...)
	sort.Strings(all)

	return &Server{
		counters:   make(map[string]int64),
		id:         id,
		self:       self,
		peers:      peers,
		allServers: all,
		leaderAddr: all[0], // initial leader is the first server in sorted order
		viewVotes:  make(map[int64]map[string]bool),
	}
}

// Acquires write lock then increments counter by one and returns new value
func (s *Server) IncrCounter(_ context.Context, req *pb.IncrRequest) (*pb.IncrResponse, error) {
	if req.CounterId == "" {
		return nil, status.Error(codes.InvalidArgument, "counter_id must not be empty")
	}

	s.mu.Lock()
	s.counters[req.CounterId]++
	val := s.counters[req.CounterId]
	s.mu.Unlock()

	return &pb.IncrResponse{NewValue: val}, nil
}

// Acquires read lock then returns current value (0 if counter doesn't exist yet)
func (s *Server) GetCounter(_ context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	if req.CounterId == "" {
		return nil, status.Error(codes.InvalidArgument, "counter_id must not be empty")
	}

	s.mu.RLock()
	val := s.counters[req.CounterId]
	s.mu.RUnlock()

	return &pb.GetResponse{Value: val}, nil
}
