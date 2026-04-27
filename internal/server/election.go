package server

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	pb "github.com/kadenchien/390-final-project/gen/counter"
	"google.golang.org/grpc"
)

const (
	heartbeatInterval = 200 * time.Millisecond
	maxMissedPings = 3
)

//ping gives confirmation that server is alive & returns current view #
func (s *Server) Ping(_ context.Context, req *pb.PingRequest) (*pb.PingResponse, error) {
	return &pb.PingResponse{Ok: true, ViewNumber: atomic.LoadInt64(&s.viewNumber)}, nil
}

//StartHeartbeat - called from main when server starts
func (s *Server) StartHeartbeat() {
	go s.runHeartbeat()
}

//runHeartbeat
//leader pinged 200ms, if 3 misses -> leader failed & initiate view change, compute new leader
func (s *Server) runHeartbeat() {
	missCount := 0

	for{
		time.Sleep(heartbeatInterval)
		leader := s.currentLeader()

		//don't ping ourself
		if leader == s.self {
			missCount = 0
			continue
		}

		if s.pingLeader(leader) {
			missCount = 0
		}else {
			missCount++
			//logging addres of server, how many misses from max
			log.Printf("[%s] missed ping to leader %s (%d/%d)", s.self, leader, missCount, maxMissedPings)
		}

		if missCount >= maxMissedPings {
			missCount = 0
			s.initiateViewChange()
		}
	}
}

//return address of current leader
func (s *Server) currentLeader() string{
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.leaderAddr
}

func (s *Server) pingLeader(addr string) bool {
	conn, ok := s.peerConns[addr]
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := pb.NewCounterServiceClient(conn).Ping(ctx, &pb.PingRequest{})
	return err == nil
}

//increment view # and compute leader -> broadcasted to all servers
func (s *Server) initiateViewChange() {
	newView := atomic.AddInt64(&s.viewNumber, 1)
	newLeader := s.allServers[newView%int64(len(s.allServers))]

	// Enter view change mode before updating leaderAddr or broadcasting
	atomic.StoreInt32(&s.inViewChange, 1)

	s.mu.Lock()
	s.leaderAddr = newLeader
	// Seed our own vote so it counts when peers' ViewChange RPCs arrive
	if s.viewVotes[newView] == nil {
		s.viewVotes[newView] = make(map[string]bool)
	}
	s.viewVotes[newView][s.self] = true
	s.mu.Unlock()

	log.Printf("[%s] initiating view change to view %d, new leader %s", s.self, newView, newLeader)

	majority := len(s.allServers)/2 + 1
	s.mu.RLock()
	votes := len(s.viewVotes[newView])
	s.mu.RUnlock()
	if votes >= majority {
		s.commitView(newView)
		return
	}

	s.broadcastViewChange(newView)
}

// ViewChange RPC handler. This is called by peers when they want to initiate a view change
// Tracks votes per view number and commits when a majority is reached
func (s *Server) ViewChange(_ context.Context, msg *pb.ViewChangeMsg) (*pb.ViewChangeAck, error) {
	currentView := atomic.LoadInt64(&s.viewNumber)

	// Reject stale messages
	if msg.ViewNumber < currentView {
		return &pb.ViewChangeAck{Accepted: false, ViewNumber: currentView}, nil
	}

	// If this is a higher view than we know about, enter view change mode
	if msg.ViewNumber > currentView {
		atomic.StoreInt32(&s.inViewChange, 1)
	}

	majority := len(s.allServers)/2 + 1

	s.mu.Lock()
	if s.viewVotes[msg.ViewNumber] == nil {
		// First time seeing this view — count ourselves as agreeing
		s.viewVotes[msg.ViewNumber] = map[string]bool{s.self: true}
	}
	s.viewVotes[msg.ViewNumber][msg.SenderId] = true
	votes := len(s.viewVotes[msg.ViewNumber])
	s.mu.Unlock()

	log.Printf("[%s] view %d has %d/%d votes", s.self, msg.ViewNumber, votes, majority)

	if votes == majority {
		s.commitView(msg.ViewNumber)
	}

	return &pb.ViewChangeAck{Accepted: true, ViewNumber: msg.ViewNumber}, nil
}

// commitView advances viewNumber to newView (if not already there), updates leaderAddr,
// and clears inViewChange so the new leader can begin serving requests.
func (s *Server) commitView(newView int64) {
	for {
		current := atomic.LoadInt64(&s.viewNumber)
		if newView < current {
			return // a newer view was already committed; nothing to do
		}
		if newView == current {
			break // initiator already set viewNumber via AddInt64; just clear the flag
		}
		if atomic.CompareAndSwapInt64(&s.viewNumber, current, newView) {
			break
		}
	}

	newLeader := s.allServers[newView%int64(len(s.allServers))]
	s.mu.Lock()
	s.leaderAddr = newLeader
	s.mu.Unlock()

	// Majority reached. This means the new leader is now ready to serve
	atomic.StoreInt32(&s.inViewChange, 0)
	log.Printf("[%s] committed view %d, new leader %s", s.self, newView, newLeader)
}

func (s *Server) broadcastViewChange(viewNumber int64) {
	for _, peer := range s.peers {
		conn, ok := s.peerConns[peer]
		if !ok {
			continue
		}
		go func(addr string, c *grpc.ClientConn) {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_, err := pb.NewCounterServiceClient(c).ViewChange(ctx, &pb.ViewChangeMsg{ViewNumber: viewNumber, SenderId: s.self})
			if err != nil {
				log.Printf("[%s] broadcastViewChange: ViewChange to %s failed: %v", s.self, addr, err)
			}
		}(peer, conn)
	}
}