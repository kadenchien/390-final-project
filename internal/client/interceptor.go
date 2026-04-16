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

// LeaderInterceptor manages the active connection to what we currently believe is the leader
type LeaderInterceptor struct {
	mu         sync.RWMutex
	currConn   *grpc.ClientConn
	leaderAddr string
}

// NewLeaderInterceptor creates the initial connection to provided address
func NewLeaderInterceptor(initialLeader string) (*LeaderInterceptor, error) {
	conn, err := grpc.NewClient(initialLeader, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &LeaderInterceptor{
		currConn:   conn,
		leaderAddr: initialLeader,
	}, nil
}

// Conn lets main application to grab the current active connection
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

// Create a new connection to the newly discovered leader.
func (l *LeaderInterceptor) rebind(newAddr string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// skip if another goroutine already updated the connection while we were waiting for the lock
	if l.leaderAddr == newAddr {
		return nil
	}

	log.Printf("[Interceptor] Rebinding to new leader: %s", newAddr)
	
	conn, err := grpc.NewClient(newAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}

	// Close the old connection to prevent resource leaks
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
			// 1. Grab the freshest connection
			l.mu.RLock()
			activeConn := l.currConn
			l.mu.RUnlock()

			// 2. Pass the request down the chain to the server
			err := invoker(ctx, method, req, reply, activeConn, opts...)

			// If it's a hard network error (like Unavailable), we return it immediately 
			// and let the go-grpc-middleware Retry interceptor handle the backoff logic
			if err != nil {
				return err
			}

			// 3. The request succeeded (no error) --> check if it was a redirect
			if incrResp, ok := reply.(*pb.IncrResponse); ok && incrResp.RedirectTo != "" {
				log.Printf("[Interceptor] Caught redirect hint. Old leader was wrong, new leader is: %s", incrResp.RedirectTo)
				
				// Rebind internal connection to the address provided by the follower
				if bindErr := l.rebind(incrResp.RedirectTo); bindErr != nil {
					return fmt.Errorf("failed to rebind: %w", bindErr)
				}
				
				// Reset the redirect field so we don't accidentally trip on stale data
				incrResp.RedirectTo = ""
				
				continue 
			}

			// reach here == we successfully hit the actual leader
			return nil 
		}

		return fmt.Errorf("exceeded max redirects (%d)", maxRedirects)
	}
}