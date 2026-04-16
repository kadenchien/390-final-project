package client

import (
	"context"
	"fmt"
	"log"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kadenchien/390-final-project/gen/counter"
)

type LeaderInterceptor struct {
	mu         sync.RWMutex
	currConn   *grpc.ClientConn
	leaderAddr string
	peers      []string
	peerIdx    int
}

func NewLeaderInterceptor(peers []string) (*LeaderInterceptor, error) {
	if len(peers) == 0 {
		return nil, fmt.Errorf("no peers provided")
	}
	initialLeader := peers[0]
	conn, err := grpc.NewClient(initialLeader, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &LeaderInterceptor{
		currConn:   conn,
		leaderAddr: initialLeader,
		peers:      peers,
		peerIdx:    0,
	}, nil
}

func (l *LeaderInterceptor) Conn() *grpc.ClientConn {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.currConn
}

func (l *LeaderInterceptor) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.currConn != nil {
		return l.currConn.Close()
	}
	return nil
}

func (l *LeaderInterceptor) rebind(newAddr string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.leaderAddr == newAddr {
		return nil
	}

	log.Printf("[Interceptor] Rebinding to new leader: %s", newAddr)
	
	conn, err := grpc.NewClient(newAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}

	if l.currConn != nil {
		l.currConn.Close()
	}

	l.currConn = conn
	l.leaderAddr = newAddr
	return nil
}

func (l *LeaderInterceptor) Unary() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		const maxRedirects = 3

		for attempt := 0; attempt < maxRedirects; attempt++ {
			l.mu.RLock()
			activeConn := l.currConn
			l.mu.RUnlock()

			err := invoker(ctx, method, req, reply, activeConn, opts...)

			if err != nil {
				l.mu.Lock()
				l.peerIdx = (l.peerIdx + 1) % len(l.peers)
				nextPeer := l.peers[l.peerIdx]
				l.mu.Unlock()

				log.Printf("[Interceptor] Connection failed. Trying backup peer to find new leader: %s", nextPeer)
				if bindErr := l.rebind(nextPeer); bindErr != nil {
					return fmt.Errorf("failed to bind to backup peer: %w", bindErr)
				}
				continue
			}

			// success --> check if it was a redirect
			if incrResp, ok := reply.(*pb.IncrResponse); ok && incrResp.RedirectTo != "" {
				log.Printf("[Interceptor] Caught redirect hint. New leader is: %s", incrResp.RedirectTo)

				if bindErr := l.rebind(incrResp.RedirectTo); bindErr != nil {
					return fmt.Errorf("failed to rebind: %w", bindErr)
				}
				
				incrResp.RedirectTo = ""
				continue 
			}

			// reach here == we successfully hit the actual leader
			return nil 
		}

		return fmt.Errorf("exceeded max redirects (%d)", maxRedirects)
	}
}