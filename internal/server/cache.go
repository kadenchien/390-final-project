package server

import (
	"fmt"
	"sync"

	pb "github.com/kadenchien/390-final-project/gen/counter"
)

//if leader dies after replication but before ack, client retransmits same requestID to new leader & new leader returns cached response

type ReplyCache struct {
	mu sync.Mutex
	cache map[string]*pb.IncrResponse
}

func newReplyCache() *ReplyCache{
	return &ReplyCache{
		cache: make(map[string]*pb.IncrResponse),
	}
}

func cacheKey(clientID string, requestID int64) string {
	return fmt.Sprintf("%s:%d", clientID, requestID)
}

//get returns cached response, nil if not found
func (c *ReplyCache) get(clientID string, requestID int64) (*pb.IncrResponse, bool){
	c.mu.Lock()
	defer c.mu.Unlock()
	resp, ok := c.cache[cacheKey(clientID, requestID)]
	return resp, ok
}

//set stores response
func (c *ReplyCache) set(clientID string, requestID int64, resp *pb.IncrResponse){
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[cacheKey(clientID, requestID)] = resp
}

