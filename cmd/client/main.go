package main

import (
	"context"
	"flag"
	"log"
	"strings"
	"time"

	pb "github.com/kadenchien/390-final-project/gen/counter"
	"github.com/kadenchien/390-final-project/internal/client"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/retry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
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
	increments := flag.Int("increments", 5, "number of times to increment (ignored in --continuous mode)")
	continuous := flag.Bool("continuous", false, "loop forever, incrementing until Ctrl-C")
	sleep      := flag.Duration("sleep", 500*time.Millisecond, "sleep between increments")
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

	c := pb.NewCounterServiceClient(conn)

	doIncr := func(i int) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resp, err := c.IncrCounter(ctx, &pb.IncrRequest{CounterId: *counterID})
		cancel()
		if err != nil {
			log.Printf("[Client] increment #%d failed (leader=%s): %v", i, leaderInterceptor.CurrentLeader(), err)
			return false
		}
		log.Printf("[Client] increment #%d → value=%d  (leader=%s)", i, resp.NewValue, leaderInterceptor.CurrentLeader())
		return true
	}

	if *continuous {
		log.Printf("[Client] continuous mode — Ctrl-C to stop (sleep=%v)", *sleep)
		for i := 1; ; i++ {
			doIncr(i)
			time.Sleep(*sleep)
		}
	} else {
		for i := range *increments {
			doIncr(i + 1)
			time.Sleep(*sleep)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := c.GetCounter(ctx, &pb.GetRequest{CounterId: *counterID})
	if err != nil {
		log.Fatalf("GetCounter: %v", err)
	}
	log.Printf("[Client] final GetCounter(%q) = %d  (leader=%s)", *counterID, got.Value, leaderInterceptor.CurrentLeader())
}
