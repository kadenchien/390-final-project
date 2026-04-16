package main

import (
	"context"
	"flag"
	"log"
	"time"
	"strings"

	pb "github.com/kadenchien/390-final-project/gen/counter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/retry"
    "google.golang.org/grpc/codes"
    "github.com/kadenchien/390-final-project/internal/client"
)

func loggingInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
    start := time.Now()
    err := invoker(ctx, method, req, reply, cc, opts...)
    log.Printf("[Client] RPC: %s | Latency: %v | Error: %v", method, time.Since(start), err)
    return err
}

func main() {
	serversStr := flag.String("servers", "localhost:50051,localhost:50052,localhost:50053,localhost:50054,localhost:50055", "comma-separated server addresses")
	counterID  := flag.String("counter", "foo", "counter name to increment")
	increments := flag.Int("increments", 5, "number of times to increment")
	flag.Parse()

	peers := strings.Split(*serversStr, ",")

    reqIDInterceptor := client.NewRequestIDInterceptor()
    leaderInterceptor, err := client.NewLeaderInterceptor(peers)
    if err != nil {
        log.Fatalf("failed to initialize leader interceptor: %v", err)
    }
    defer leaderInterceptor.Close()

    retryOpts := []retry.CallOption{
        retry.WithMax(5),
        retry.WithCodes(codes.Unavailable),
        retry.WithBackoff(retry.BackoffExponentialWithJitter(50*time.Millisecond, 0.10)),
    }

	conn, err := grpc.NewClient(peers[0], 
        grpc.WithTransportCredentials(insecure.NewCredentials()),
        grpc.WithChainUnaryInterceptor(
            loggingInterceptor,
            reqIDInterceptor.Unary(),
            leaderInterceptor.Unary(),
            retry.UnaryClientInterceptor(retryOpts...),
        ),
    )

	if err != nil {
		log.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	client := pb.NewCounterServiceClient(conn)

	for i := range *increments {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := client.IncrCounter(ctx, &pb.IncrRequest{CounterId: *counterID})
		cancel()
		if err != nil {
			log.Fatalf("IncrCounter(%d): %v", i+1, err)
		}
		log.Printf("increment #%d → %d", i+1, resp.NewValue)
		time.Sleep(500 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := client.GetCounter(ctx, &pb.GetRequest{CounterId: *counterID})
	if err != nil {
		log.Fatalf("GetCounter: %v", err)
	}
	log.Printf("final GetCounter(%q) = %d", *counterID, got.Value)
}
