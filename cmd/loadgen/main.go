package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/retry"
	pb "github.com/kadenchien/390-final-project/gen/counter"
	clientpkg "github.com/kadenchien/390-final-project/internal/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type resultRow struct {
	OpID         int64
	WorkerID     int
	Operation    int
	StartNS      int64
	EndNS        int64
	Status       string
	AttemptCount int64
	LeaderBefore string
	LeaderAfter  string
	ErrorText    string
}

type runMeta struct {
	Servers             []string `json:"servers"`
	Counter             string   `json:"counter"`
	Workers             int      `json:"workers"`
	IncrementsPerWorker int      `json:"increments_per_worker"`
	ThinkTime           string   `json:"think_time"`
	Timeout             string   `json:"timeout"`
	OutputCSV           string   `json:"output_csv"`
	KillAfter           int64    `json:"kill_after"`
	KillTarget          string   `json:"kill_target"`
	KillSignal          string   `json:"kill_signal"`
	KillAddress         string   `json:"kill_address"`
	StartedAtNS         int64    `json:"started_at_ns"`
	FinishedAtNS        int64    `json:"finished_at_ns"`
	KillTriggeredAtNS   int64    `json:"kill_triggered_at_ns,omitempty"`
	KillResolvedAddr    string   `json:"kill_resolved_addr,omitempty"`
	KillPID             int      `json:"kill_pid,omitempty"`
	KillError           string   `json:"kill_error,omitempty"`
	SuccessCount        int64    `json:"success_count"`
	FailureCount        int64    `json:"failure_count"`
	FinalCounterValue   int64    `json:"final_counter_value"`
	FinalCounterError   string   `json:"final_counter_error,omitempty"`
}

type killEvent struct {
	TriggeredAtNS int64
	Address       string
	PID           int
	Err           error
}

type faultInjector struct {
	leader       *clientpkg.LeaderInterceptor
	peers        []string
	target       string
	explicitAddr string
	signalName   string
	signalValue  syscall.Signal
	once         sync.Once
	result       killEvent
}

func newFaultInjector(leader *clientpkg.LeaderInterceptor, peers []string, target, explicitAddr, signalName string) (*faultInjector, error) {
	signalValue, err := parseSignal(signalName)
	if err != nil {
		return nil, err
	}

	return &faultInjector{
		leader:       leader,
		peers:        peers,
		target:       target,
		explicitAddr: explicitAddr,
		signalName:   strings.ToUpper(signalName),
		signalValue:  signalValue,
	}, nil
}

func (f *faultInjector) Trigger() killEvent {
	f.once.Do(func() {
		f.result.TriggeredAtNS = time.Now().UnixNano()

		addr, err := f.resolveTarget()
		if err != nil {
			f.result.Err = err
			return
		}
		f.result.Address = addr

		pid, err := findListeningPID(addr)
		if err != nil {
			f.result.Err = err
			return
		}
		f.result.PID = pid

		proc, err := os.FindProcess(pid)
		if err != nil {
			f.result.Err = err
			return
		}

		if err := proc.Signal(f.signalValue); err != nil {
			f.result.Err = err
		}
	})

	return f.result
}

func (f *faultInjector) Result() killEvent {
	return f.result
}

func (f *faultInjector) resolveTarget() (string, error) {
	if f.explicitAddr != "" {
		return f.explicitAddr, nil
	}

	leaderAddr := f.leader.CurrentLeader()
	switch f.target {
	case "leader":
		if leaderAddr == "" {
			return "", errors.New("could not determine current leader")
		}
		return leaderAddr, nil
	case "follower":
		for _, peer := range f.peers {
			if peer != leaderAddr {
				return peer, nil
			}
		}
		return "", errors.New("no follower available to kill")
	default:
		return "", fmt.Errorf("unsupported kill target %q", f.target)
	}
}

func parseSignal(name string) (syscall.Signal, error) {
	switch strings.ToLower(name) {
	case "term", "sigterm":
		return syscall.SIGTERM, nil
	case "kill", "sigkill":
		return syscall.SIGKILL, nil
	default:
		return 0, fmt.Errorf("unsupported kill signal %q (use term or kill)", name)
	}
}

func findListeningPID(addr string) (int, error) {
	port, err := extractPort(addr)
	if err != nil {
		return 0, err
	}

	out, err := exec.Command("lsof", "-ti", fmt.Sprintf("tcp:%s", port), "-s", "tcp:LISTEN").Output()
	if err != nil {
		return 0, fmt.Errorf("failed to locate process on %s: %w", addr, err)
	}

	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		return 0, fmt.Errorf("no listening process found on %s", addr)
	}

	pid, err := strconv.Atoi(lines[0])
	if err != nil {
		return 0, fmt.Errorf("invalid pid %q for %s: %w", lines[0], addr, err)
	}
	return pid, nil
}

func extractPort(addr string) (string, error) {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port, nil
	}

	idx := strings.LastIndex(addr, ":")
	if idx == -1 || idx == len(addr)-1 {
		return "", fmt.Errorf("address %q does not contain a port", addr)
	}
	return addr[idx+1:], nil
}

func parsePeers(raw string) []string {
	var peers []string
	for _, item := range strings.Split(raw, ",") {
		if peer := strings.TrimSpace(item); peer != "" {
			peers = append(peers, peer)
		}
	}
	return peers
}

func statusString(err error) string {
	if err == nil {
		return codes.OK.String()
	}
	if s, ok := status.FromError(err); ok {
		return s.Code().String()
	}
	return "ERROR"
}

func main() {
	serversStr := flag.String("servers", "localhost:50051,localhost:50052,localhost:50053,localhost:50054,localhost:50055", "comma-separated server addresses")
	counterID := flag.String("counter", "foo", "counter name to increment")
	workers := flag.Int("workers", 8, "number of concurrent goroutines")
	increments := flag.Int("increments", 100, "number of sequential increments per worker")
	thinkTime := flag.Duration("think-time", 0, "sleep between increments in each worker")
	timeout := flag.Duration("timeout", 3*time.Second, "per-RPC timeout")
	output := flag.String("output", "results/loadgen.csv", "path to the output CSV")
	killAfter := flag.Int64("kill-after", 0, "kill a server after this many logical RPCs complete (0 disables fault injection)")
	killSignal := flag.String("kill-signal", "term", "fault-injection signal: term or kill")
	killTarget := flag.String("kill-target", "leader", "which server to kill: leader or follower")
	killAddress := flag.String("kill-address", "", "override kill target with an explicit server address")
	flag.Parse()

	if *workers <= 0 {
		log.Fatal("workers must be > 0")
	}
	if *increments <= 0 {
		log.Fatal("increments must be > 0")
	}

	peers := parsePeers(*serversStr)
	if len(peers) == 0 {
		log.Fatal("at least one server address is required")
	}

	if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
		log.Fatalf("failed to create output directory: %v", err)
	}

	csvFile, err := os.Create(*output)
	if err != nil {
		log.Fatalf("failed to create CSV output: %v", err)
	}
	defer csvFile.Close()

	writer := csv.NewWriter(csvFile)
	if err := writer.Write([]string{
		"op_id",
		"worker_id",
		"operation",
		"start_ns",
		"end_ns",
		"latency_ms",
		"status",
		"attempt_count",
		"leader_before",
		"leader_after",
		"error",
	}); err != nil {
		log.Fatalf("failed to write CSV header: %v", err)
	}

	reqIDInterceptor := clientpkg.NewRequestIDInterceptor()
	leaderInterceptor, err := clientpkg.NewLeaderInterceptor(peers)
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
			reqIDInterceptor.Unary(),
			leaderInterceptor.Unary(),
			retry.UnaryClientInterceptor(retryOpts...),
			clientpkg.AttemptCountingInterceptor(),
		),
	)
	if err != nil {
		log.Fatalf("failed to create gRPC client: %v", err)
	}
	defer conn.Close()

	client := pb.NewCounterServiceClient(conn)
	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	injector, err := newFaultInjector(leaderInterceptor, peers, *killTarget, *killAddress, *killSignal)
	if err != nil {
		log.Fatalf("failed to initialize fault injector: %v", err)
	}

	meta := runMeta{
		Servers:             peers,
		Counter:             *counterID,
		Workers:             *workers,
		IncrementsPerWorker: *increments,
		ThinkTime:           thinkTime.String(),
		Timeout:             timeout.String(),
		OutputCSV:           *output,
		KillAfter:           *killAfter,
		KillTarget:          *killTarget,
		KillSignal:          strings.ToUpper(*killSignal),
		KillAddress:         *killAddress,
		StartedAtNS:         time.Now().UnixNano(),
	}

	log.Printf("[loadgen] workers=%d increments/worker=%d counter=%q kill-after=%d target=%s signal=%s",
		*workers, *increments, *counterID, *killAfter, *killTarget, strings.ToUpper(*killSignal))

	rows := make(chan resultRow, 1024)

	var started atomic.Int64
	var completed atomic.Int64
	var successCount atomic.Int64
	var failureCount atomic.Int64
	var killTriggered atomic.Bool
	var workersWG sync.WaitGroup

	for workerID := 1; workerID <= *workers; workerID++ {
		workersWG.Add(1)
		go func(workerID int) {
			defer workersWG.Done()

			for op := 1; op <= *increments; op++ {
				if err := runCtx.Err(); err != nil {
					return
				}

				opID := started.Add(1)
				tracker := clientpkg.NewAttemptTracker()
				rpcCtx, cancel := context.WithTimeout(runCtx, *timeout)
				rpcCtx = clientpkg.WithAttemptTracker(rpcCtx, tracker)

				start := time.Now()
				leaderBefore := leaderInterceptor.CurrentLeader()
				_, err := client.IncrCounter(rpcCtx, &pb.IncrRequest{CounterId: *counterID})
				end := time.Now()
				cancel()

				row := resultRow{
					OpID:         opID,
					WorkerID:     workerID,
					Operation:    op,
					StartNS:      start.UnixNano(),
					EndNS:        end.UnixNano(),
					Status:       statusString(err),
					AttemptCount: tracker.Count(),
					LeaderBefore: leaderBefore,
					LeaderAfter:  leaderInterceptor.CurrentLeader(),
				}
				if err != nil {
					row.ErrorText = err.Error()
					failureCount.Add(1)
				} else {
					successCount.Add(1)
				}

				rows <- row

				completedNow := completed.Add(1)
				if *killAfter > 0 && completedNow == *killAfter {
					killTriggered.Store(true)
					event := injector.Trigger()
					if event.Err != nil {
						log.Printf("[loadgen] fault injection failed: %v", event.Err)
					} else {
						log.Printf("[loadgen] sent SIG%s to %s (pid=%d) after %d completed RPCs",
							injector.signalName, event.Address, event.PID, *killAfter)
					}
				}

				if *thinkTime > 0 {
					select {
					case <-runCtx.Done():
						return
					case <-time.After(*thinkTime):
					}
				}
			}
		}(workerID)
	}

	go func() {
		workersWG.Wait()
		close(rows)
	}()

	var csvRows int64
	for row := range rows {
		latencyMS := float64(row.EndNS-row.StartNS) / float64(time.Millisecond)
		record := []string{
			strconv.FormatInt(row.OpID, 10),
			strconv.Itoa(row.WorkerID),
			strconv.Itoa(row.Operation),
			strconv.FormatInt(row.StartNS, 10),
			strconv.FormatInt(row.EndNS, 10),
			fmt.Sprintf("%.3f", latencyMS),
			row.Status,
			strconv.FormatInt(row.AttemptCount, 10),
			row.LeaderBefore,
			row.LeaderAfter,
			row.ErrorText,
		}

		if err := writer.Write(record); err != nil {
			log.Fatalf("failed to write CSV row: %v", err)
		}

		csvRows++
		if csvRows%100 == 0 {
			writer.Flush()
			if err := writer.Error(); err != nil {
				log.Fatalf("failed to flush CSV: %v", err)
			}
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		log.Fatalf("failed to finalize CSV: %v", err)
	}

	meta.FinishedAtNS = time.Now().UnixNano()
	meta.SuccessCount = successCount.Load()
	meta.FailureCount = failureCount.Load()

	if meta.KillAfter > 0 && killTriggered.Load() {
		event := injector.Result()
		meta.KillTriggeredAtNS = event.TriggeredAtNS
		meta.KillResolvedAddr = event.Address
		meta.KillPID = event.PID
		if event.Err != nil {
			meta.KillError = event.Err.Error()
		}
	}

	verifyCtx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	finalResp, err := client.GetCounter(verifyCtx, &pb.GetRequest{CounterId: *counterID})
	if err != nil {
		meta.FinalCounterError = err.Error()
	} else {
		meta.FinalCounterValue = finalResp.Value
	}

	metaPath := strings.TrimSuffix(*output, filepath.Ext(*output)) + ".meta.json"
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		log.Fatalf("failed to marshal run metadata: %v", err)
	}
	if err := os.WriteFile(metaPath, metaBytes, 0o644); err != nil {
		log.Fatalf("failed to write run metadata: %v", err)
	}

	log.Printf("[loadgen] wrote %d rows to %s", csvRows, *output)
	log.Printf("[loadgen] success=%d failure=%d final-counter=%d",
		meta.SuccessCount, meta.FailureCount, meta.FinalCounterValue)
	if meta.FinalCounterError != "" {
		log.Printf("[loadgen] final GetCounter failed: %s", meta.FinalCounterError)
	}
	if meta.KillAfter > 0 && meta.KillError != "" {
		log.Printf("[loadgen] kill metadata recorded an error: %s", meta.KillError)
	}
}
