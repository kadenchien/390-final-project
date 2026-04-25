package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type sample struct {
	StartNS      int64
	EndNS        int64
	LatencyMS    float64
	Status       string
	AttemptCount int64
}

type runMeta struct {
	StartedAtNS       int64  `json:"started_at_ns"`
	FinishedAtNS      int64  `json:"finished_at_ns"`
	KillTriggeredAtNS int64  `json:"kill_triggered_at_ns"`
	KillResolvedAddr  string `json:"kill_resolved_addr"`
	KillError         string `json:"kill_error"`
}

func main() {
	input := flag.String("input", "results/loadgen.csv", "path to the measurement CSV")
	metaPath := flag.String("meta", "", "path to the run metadata JSON (defaults to <input>.meta.json)")
	cdfOut := flag.String("cdf-out", "", "path to the output latency CDF SVG")
	timelineOut := flag.String("timeline-out", "", "path to the output latency timeline SVG")
	flag.Parse()

	if *metaPath == "" {
		*metaPath = strings.TrimSuffix(*input, filepath.Ext(*input)) + ".meta.json"
	}
	if *cdfOut == "" {
		*cdfOut = strings.TrimSuffix(*input, filepath.Ext(*input)) + ".cdf.svg"
	}
	if *timelineOut == "" {
		*timelineOut = strings.TrimSuffix(*input, filepath.Ext(*input)) + ".timeline.svg"
	}

	samples, err := readSamples(*input)
	if err != nil {
		log.Fatalf("failed to read samples: %v", err)
	}
	if len(samples) == 0 {
		log.Fatal("no samples found in CSV")
	}

	meta, _ := readMeta(*metaPath)

	var successLatencies []float64
	var errorCount int
	for _, s := range samples {
		if s.Status == "OK" {
			successLatencies = append(successLatencies, s.LatencyMS)
		} else {
			errorCount++
		}
	}
	sort.Float64s(successLatencies)

	fmt.Printf("rows: %d\n", len(samples))
	fmt.Printf("successes: %d\n", len(successLatencies))
	fmt.Printf("failures: %d\n", errorCount)
	if len(successLatencies) > 0 {
		fmt.Printf("p50: %.3f ms\n", percentile(successLatencies, 0.50))
		fmt.Printf("p95: %.3f ms\n", percentile(successLatencies, 0.95))
		fmt.Printf("p99: %.3f ms\n", percentile(successLatencies, 0.99))
	}

	failoverEndNS := int64(0)
	failoverLatencyMS := 0.0
	if meta != nil && meta.KillTriggeredAtNS > 0 {
		failoverEndNS = firstSuccessfulCompletionAfter(samples, meta.KillTriggeredAtNS)
		if failoverEndNS > 0 {
			failoverLatencyMS = float64(failoverEndNS-meta.KillTriggeredAtNS) / float64(time.Millisecond)
			fmt.Printf("kill_at: %d\n", meta.KillTriggeredAtNS)
			fmt.Printf("kill_target: %s\n", meta.KillResolvedAddr)
			fmt.Printf("failover_latency: %.3f ms\n", failoverLatencyMS)
		}
		if meta.KillError != "" {
			fmt.Printf("kill_error: %s\n", meta.KillError)
		}
	}

	if err := writeCDFSVG(*cdfOut, successLatencies, failoverLatencyMS); err != nil {
		log.Fatalf("failed to write CDF SVG: %v", err)
	}
	if err := writeTimelineSVG(*timelineOut, samples, meta, failoverEndNS); err != nil {
		log.Fatalf("failed to write timeline SVG: %v", err)
	}

	fmt.Printf("cdf_svg: %s\n", *cdfOut)
	fmt.Printf("timeline_svg: %s\n", *timelineOut)
}

func readSamples(path string) ([]sample, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	rows, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) < 2 {
		return nil, nil
	}

	index := make(map[string]int)
	for i, field := range rows[0] {
		index[field] = i
	}

	var out []sample
	for _, row := range rows[1:] {
		startNS, err := strconv.ParseInt(row[index["start_ns"]], 10, 64)
		if err != nil {
			return nil, err
		}
		endNS, err := strconv.ParseInt(row[index["end_ns"]], 10, 64)
		if err != nil {
			return nil, err
		}
		latencyMS, err := strconv.ParseFloat(row[index["latency_ms"]], 64)
		if err != nil {
			return nil, err
		}
		attemptCount, err := strconv.ParseInt(row[index["attempt_count"]], 10, 64)
		if err != nil {
			return nil, err
		}

		out = append(out, sample{
			StartNS:      startNS,
			EndNS:        endNS,
			LatencyMS:    latencyMS,
			Status:       row[index["status"]],
			AttemptCount: attemptCount,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].EndNS < out[j].EndNS
	})
	return out, nil
}

func readMeta(path string) (*runMeta, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var meta runMeta
	if err := json.Unmarshal(bytes, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}

	pos := p * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}

	weight := pos - float64(lo)
	return sorted[lo] + (sorted[hi]-sorted[lo])*weight
}

func firstSuccessfulCompletionAfter(samples []sample, killNS int64) int64 {
	for _, sample := range samples {
		if sample.EndNS >= killNS && sample.Status == "OK" {
			return sample.EndNS
		}
	}
	return 0
}

func writeCDFSVG(path string, latencies []float64, failoverLatencyMS float64) error {
	if len(latencies) == 0 {
		return os.WriteFile(path, []byte("<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"900\" height=\"520\"></svg>"), 0o644)
	}

	const (
		width  = 900
		height = 520
		left   = 70
		right  = 30
		top    = 40
		bottom = 60
	)

	xMax := latencies[len(latencies)-1]
	if xMax < 1 {
		xMax = 1
	}
	xMax *= 1.05

	var points strings.Builder
	for i, latency := range latencies {
		x := scale(latency, 0, xMax, left, width-right)
		y := scale(float64(i+1)/float64(len(latencies)), 0, 1, height-bottom, top)
		fmt.Fprintf(&points, "%.2f,%.2f ", x, y)
	}

	failoverX := -1.0
	if failoverLatencyMS > 0 {
		failoverX = scale(failoverLatencyMS, 0, xMax, left, width-right)
	}

	svg := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">
  <rect width="100%%" height="100%%" fill="#fffdf7"/>
  <text x="%d" y="24" font-family="Menlo, monospace" font-size="18" fill="#1f2937">Latency CDF</text>
  <line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#111827" stroke-width="1.5"/>
  <line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#111827" stroke-width="1.5"/>
  <polyline fill="none" stroke="#0f766e" stroke-width="2" points="%s"/>
  <text x="%d" y="%d" font-family="Menlo, monospace" font-size="12" fill="#374151">0 ms</text>
  <text x="%d" y="%d" text-anchor="end" font-family="Menlo, monospace" font-size="12" fill="#374151">%.1f ms</text>
  <text x="%d" y="%d" font-family="Menlo, monospace" font-size="12" fill="#374151">0%%</text>
  <text x="%d" y="%d" font-family="Menlo, monospace" font-size="12" fill="#374151">100%%</text>
  <text x="%d" y="%d" font-family="Menlo, monospace" font-size="12" fill="#374151">Latency (ms)</text>
  <text x="%d" y="%d" transform="rotate(-90 %d %d)" font-family="Menlo, monospace" font-size="12" fill="#374151">CDF</text>
  %s
</svg>`,
		width, height, width, height,
		left,
		left, height-bottom, width-right, height-bottom,
		left, height-bottom, left, top,
		points.String(),
		left, height-bottom+20,
		width-right, height-bottom+20, xMax,
		left-28, height-bottom,
		left-38, top+4,
		(width)/2, height-18,
		20, (height)/2, 20, (height)/2,
		failoverLabel(failoverX, top, height-bottom, failoverLatencyMS),
	)

	return os.WriteFile(path, []byte(svg), 0o644)
}

func failoverLabel(x float64, top, bottom int, failoverLatencyMS float64) string {
	if x <= 0 || failoverLatencyMS <= 0 {
		return ""
	}

	return fmt.Sprintf(
		`<line x1="%.2f" y1="%d" x2="%.2f" y2="%d" stroke="#dc2626" stroke-width="1.5" stroke-dasharray="6 4"/>
  <text x="%.2f" y="%d" font-family="Menlo, monospace" font-size="12" fill="#b91c1c">failover %.1f ms</text>`,
		x, top, x, bottom, x+8, top+14, failoverLatencyMS,
	)
}

func writeTimelineSVG(path string, samples []sample, meta *runMeta, failoverEndNS int64) error {
	const (
		width  = 900
		height = 520
		left   = 70
		right  = 30
		top    = 40
		bottom = 60
	)

	startNS := samples[0].StartNS
	if meta != nil && meta.StartedAtNS > 0 {
		startNS = meta.StartedAtNS
	}
	endNS := samples[len(samples)-1].EndNS
	maxLatency := 0.0
	for _, sample := range samples {
		if sample.LatencyMS > maxLatency {
			maxLatency = sample.LatencyMS
		}
	}
	if maxLatency < 1 {
		maxLatency = 1
	}
	maxLatency *= 1.1

	var points strings.Builder
	for _, sample := range samples {
		x := scale(float64(sample.EndNS-startNS)/float64(time.Millisecond), 0, float64(endNS-startNS)/float64(time.Millisecond), left, width-right)
		y := scale(sample.LatencyMS, 0, maxLatency, height-bottom, top)
		color := "#2563eb"
		if sample.Status != "OK" {
			color = "#dc2626"
		}
		fmt.Fprintf(&points, `<circle cx="%.2f" cy="%.2f" r="3" fill="%s"/>`, x, y, color)
	}

	window := ""
	if meta != nil && meta.KillTriggeredAtNS > 0 && failoverEndNS > meta.KillTriggeredAtNS {
		x1 := scale(float64(meta.KillTriggeredAtNS-startNS)/float64(time.Millisecond), 0, float64(endNS-startNS)/float64(time.Millisecond), left, width-right)
		x2 := scale(float64(failoverEndNS-startNS)/float64(time.Millisecond), 0, float64(endNS-startNS)/float64(time.Millisecond), left, width-right)
		window = fmt.Sprintf(
			`<rect x="%.2f" y="%d" width="%.2f" height="%d" fill="#fca5a5" fill-opacity="0.25"/>
  <text x="%.2f" y="%d" font-family="Menlo, monospace" font-size="12" fill="#991b1b">failover window</text>`,
			x1, top, x2-x1, height-top-bottom, x1+6, top+14,
		)
	}

	durationMS := float64(endNS-startNS) / float64(time.Millisecond)
	svg := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">
  <rect width="100%%" height="100%%" fill="#fffdf7"/>
  <text x="%d" y="24" font-family="Menlo, monospace" font-size="18" fill="#1f2937">Latency Timeline</text>
  %s
  <line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#111827" stroke-width="1.5"/>
  <line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#111827" stroke-width="1.5"/>
  %s
  <text x="%d" y="%d" font-family="Menlo, monospace" font-size="12" fill="#374151">0 ms</text>
  <text x="%d" y="%d" text-anchor="end" font-family="Menlo, monospace" font-size="12" fill="#374151">%.1f ms</text>
  <text x="%d" y="%d" font-family="Menlo, monospace" font-size="12" fill="#374151">0 ms</text>
  <text x="%d" y="%d" font-family="Menlo, monospace" font-size="12" fill="#374151">%.1f ms</text>
  <text x="%d" y="%d" font-family="Menlo, monospace" font-size="12" fill="#374151">Time Since Start (ms)</text>
  <text x="%d" y="%d" transform="rotate(-90 %d %d)" font-family="Menlo, monospace" font-size="12" fill="#374151">Latency (ms)</text>
</svg>`,
		width, height, width, height,
		left,
		window,
		left, height-bottom, width-right, height-bottom,
		left, height-bottom, left, top,
		points.String(),
		left, height-bottom+20,
		width-right, height-bottom+20, durationMS,
		left-34, height-bottom,
		left-48, top+4, maxLatency,
		(width)/2, height-18,
		20, (height)/2, 20, (height)/2,
	)

	return os.WriteFile(path, []byte(svg), 0o644)
}

func scale(value, min, max float64, outMin, outMax int) float64 {
	if max <= min {
		return float64(outMin)
	}
	ratio := (value - min) / (max - min)
	return float64(outMin) + ratio*float64(outMax-outMin)
}
