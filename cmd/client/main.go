package main

import (
	"context"
	"flag"
	"log"
	"time"

	pb "github.com/kadenchien/390-final-project/gen/counter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	server     := flag.String("server", "localhost:50051", "server address")
	counterID  := flag.String("counter", "foo", "counter name to increment")
	increments := flag.Int("increments", 5, "number of times to increment")
	flag.Parse()

	conn, err := grpc.NewClient(*server, grpc.WithTransportCredentials(insecure.NewCredentials()))
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
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := client.GetCounter(ctx, &pb.GetRequest{CounterId: *counterID})
	if err != nil {
		log.Fatalf("GetCounter: %v", err)
	}
	log.Printf("final GetCounter(%q) = %d", *counterID, got.Value)
}
