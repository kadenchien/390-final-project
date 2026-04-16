package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"strings"

	pb "github.com/kadenchien/390-final-project/gen/counter"
	"github.com/kadenchien/390-final-project/internal/server"
	"google.golang.org/grpc"
)

// accept a port flag (default 50051), id, peers flag, and creates a grpc.Server, then registers it and starts listening
func main() {
	id:= flag.Int("id", 1, "server ID (1-based)")
	port := flag.Int("port", 50051, "port to listen on")
	peers := flag.String("peers", "", "comma seperated addresses of peer")
	flag.Parse()

	var peerList []string
	if *peers != "" {
		for _, p := range strings.Split(*peers, ",") {
			if addr := strings.TrimSpace(p); addr != ""{
				peerList = append(peerList,addr)
			}
		}
	}
	self := fmt.Sprintf("localhost:%d", *port)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterCounterServiceServer(grpcServer, server.New(*id, self,))

	log.Printf("server listening on :%d", *port)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
