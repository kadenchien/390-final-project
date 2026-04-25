package client

import (
	"context"
	"sync/atomic"

	"google.golang.org/grpc"
)

type attemptTrackerKey struct{}

type AttemptTracker struct {
	attempts atomic.Int64
}

func NewAttemptTracker() *AttemptTracker {
	return &AttemptTracker{}
}

func WithAttemptTracker(ctx context.Context, tracker *AttemptTracker) context.Context {
	return context.WithValue(ctx, attemptTrackerKey{}, tracker)
}

func (t *AttemptTracker) Count() int64 {
	if t == nil {
		return 0
	}
	return t.attempts.Load()
}

func AttemptCountingInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if tracker, ok := ctx.Value(attemptTrackerKey{}).(*AttemptTracker); ok && tracker != nil {
			tracker.attempts.Add(1)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
