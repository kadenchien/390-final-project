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

func NewRequestIDInterceptor() *RequestIDInterceptor {
	return &RequestIDInterceptor{
		clientID: uuid.New().String(),add
	}
}

func (i *RequestIDInterceptor) Unary() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		
		if incrReq, ok := req.(*pb.IncrRequest); ok {
			if incrReq.ClientId == "" {
				incrReq.ClientId = i.clientID
				incrReq.RequestId = i.requestID.Add(1)
			}
		}

		return invoker(ctx, method, req, reply, cc, opts...)
	}
}