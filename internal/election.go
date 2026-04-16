package server

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	pb "github.com/kadenchien/390-final-project/gen/counter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	heartbeatInterval = 200 * time.Millisecond
	maxMissedPings = 3
)

//ping gives confirmation that server is alive & returns current view #
func (s *Server) Ping(_ context.Context, req *pb.PingRequest) (*pb.PingResponse, error) {
	return &pb.PingResponse{OK: true, ViewNumber: atomic.LoadInt64(&s.viewNumber)}, nil
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

		if pingLeader(leader) {
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

//dial leaderaddr and calls ping, true if response else false
func pingLeader(addr string) bool{
	//150ms is intentionally shorter than the 200ms time that the leader has but we can adjust accordingly, shouldn't be problem w local stuff
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil{
		return false
	}
	defer conn.Close()

	_, err = pb.NewCounterServiceClient(conn).Ping(ctx, &pb.PingRequest{})
	return err == nil
}

//increment view # and compute leader -> broadcasted to all servers
func (s *Server) initiateViewChange() {
	newView := atomic.AddInt64(&s.viewNumber, 1)
	newLeader := s.allServers[newView%int64(len(s.allServers))]

	s.mu.Lock()
	s.leaderAddr = newLeader
	s.mu.Unlock()

	log.Printf("[%s] initiating view change to view %d, new leader %s", s.self, newView, newLeader)

	s.broadcastViewChange(newView)
}

//sends ViewChange RPC to peer w new view #
func (s *Server) broadcastViewChange(viewNumber int64){
	for _,peer := range s.peers{
		go func(addr string){
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				log.Printf("[%s] broadcastViewChange: failed to dial %s: %v", s.self, addr, err)
				return
			}
			defer conn.Close()

			_, err = pb.NewCounterServiceClient(conn).ViewChange(ctx, &pb.ViewChangeMsg{ViewNumber: viewNumber, SenderId: s.self})
			if err != nil{
				log.Printf("[%s] broadcastViewChange: ViewChange to %s failed: %v", s.self, addr, err)
			}
		}(peer)
	}
}