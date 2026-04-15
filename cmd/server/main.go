package main

import (
	"flag"
	"fmt"
	"log"
	"net"

	pb "github.com/kadenchien/390-final-project/gen/counter"
	"github.com/kadenchien/390-final-project/internal/server"
	"google.golang.org/grpc"
)

// accept a port flag (default 50051) and creates a grpc.Server, then registers it and starts listening
func main() {
	port := flag.Int("port", 50051, "port to listen on")
	flag.Parse()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterCounterServiceServer(grpcServer, server.New())

	log.Printf("server listening on :%d", *port)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
