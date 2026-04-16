package client

import (
	"context"
	"sync/atomic"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	pb "github.com/kadenchien/390-final-project/gen/counter" 
)

type RequestIDInterceptor struct {
	clientID  string
	requestID atomic.Int64
}

// NewRequestIDInterceptor creates a new interceptor with a unique UUID for this client session
func NewRequestIDInterceptor() *RequestIDInterceptor {
	return &RequestIDInterceptor{
		clientID: uuid.New().String(),
	}
}

// Unary returns the gRPC interceptor function
func (i *RequestIDInterceptor) Unary() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		
		// type-assert the generic req to see if it's an IncrRequest
		if incrReq, ok := req.(*pb.IncrRequest); ok {
			// Only inject if the fields are empty 
			// If a lower-level interceptor retries the exact same request object, 
			// keeps same ID
			if incrReq.ClientId == "" {
				incrReq.ClientId = i.clientID
				incrReq.RequestId = i.requestID.Add(1)
			}
		}

		// Pass request down the chain
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}