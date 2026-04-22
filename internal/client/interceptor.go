package client

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

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

func (l *LeaderInterceptor) CurrentLeader() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.leaderAddr
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

// isTransportFailure returns true when the error is a TCP-level connection
// failure (server is down) rather than a gRPC status returned by the server
// (e.g. "view change in progress").
func isTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	s, ok := status.FromError(err)
	if !ok {
		return true
	}
	if s.Code() != codes.Unavailable {
		return false
	}
	msg := s.Message()
	return strings.Contains(msg, "connection error") ||
		strings.Contains(msg, "connection refused")
}

func (l *LeaderInterceptor) Unary() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		const maxAttempts = 15
		dead := make(map[string]bool)

		for attempt := 0; attempt < maxAttempts; attempt++ {
			l.mu.RLock()
			activeConn := l.currConn
			currentAddr := l.leaderAddr
			l.mu.RUnlock()

			err := invoker(ctx, method, req, reply, activeConn, opts...)

			if err != nil {
				if isTransportFailure(err) {
					// Server is truly unreachable: mark it and move to a live peer.
					dead[currentAddr] = true
					next := l.nextAlivePeer(dead)
					if next == "" {
						return fmt.Errorf("all peers unreachable")
					}
					log.Printf("[Interceptor] Connection failed. Trying backup peer to find new leader: %s", next)
					if bindErr := l.rebind(next); bindErr != nil {
						return fmt.Errorf("failed to bind to backup peer: %w", bindErr)
					}
				} else {
					// Server is alive but temporarily unavailable (e.g. view change
					// in progress). Wait briefly and retry the same peer — it will
					// clear the flag once the election commits.
					time.Sleep(100 * time.Millisecond)
				}
				continue
			}

			// Success — check for a redirect hint.
			var redirectTo string
			switch r := reply.(type) {
			case *pb.IncrResponse:
				redirectTo = r.RedirectTo
				r.RedirectTo = ""
			case *pb.GetResponse:
				redirectTo = r.RedirectTo
				r.RedirectTo = ""
			}

			if redirectTo != "" {
				if dead[redirectTo] {
					// The follower is still pointing at the dead leader; the
					// election hasn't finished on this peer yet. Wait and retry
					// it — it will eventually redirect us to the real new leader.
					log.Printf("[Interceptor] Redirect to unreachable %s; waiting for election...", redirectTo)
					time.Sleep(100 * time.Millisecond)
				} else {
					log.Printf("[Interceptor] Caught redirect hint. New leader is: %s", redirectTo)
					if bindErr := l.rebind(redirectTo); bindErr != nil {
						return fmt.Errorf("failed to rebind: %w", bindErr)
					}
				}
				continue
			}

			// Reached the actual leader.
			return nil
		}

		return fmt.Errorf("exceeded max attempts (%d)", maxAttempts)
	}
}

// nextAlivePeer returns the next peer not in the dead set, advancing peerIdx.
func (l *LeaderInterceptor) nextAlivePeer(dead map[string]bool) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for range l.peers {
		l.peerIdx = (l.peerIdx + 1) % len(l.peers)
		candidate := l.peers[l.peerIdx]
		if !dead[candidate] {
			return candidate
		}
	}
	return ""
}
