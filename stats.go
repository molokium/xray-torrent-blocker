package main

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"sync/atomic"
	"time"
)

// runtimeStats is only touched when -perf is enabled.
type runtimeStats struct {
	start time.Time

	lines       atomic.Int64
	detections  atomic.Int64
	parseErrors atomic.Int64
	queueDrops  atomic.Int64
	parseNanos  atomic.Int64
}

func newRuntimeStats() *runtimeStats {
	return &runtimeStats{start: time.Now()}
}

func (s *runtimeStats) snapshot() string {
	lines := s.lines.Load()
	elapsed := time.Since(s.start)
	perSecond := 0.0
	if elapsed > 0 {
		perSecond = float64(lines) / elapsed.Seconds()
	}
	avgParse := time.Duration(0)
	if lines > 0 {
		avgParse = time.Duration(s.parseNanos.Load() / lines)
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return fmt.Sprintf(
		"lines=%d detections=%d parse_errors=%d queue_drops=%d lines_per_sec=%.0f avg_parse=%s goroutines=%d heap_mb=%d gc=%d uptime=%s",
		lines,
		s.detections.Load(),
		s.parseErrors.Load(),
		s.queueDrops.Load(),
		perSecond,
		avgParse.Round(time.Nanosecond),
		runtime.NumGoroutine(),
		mem.HeapInuse/(1024*1024),
		mem.NumGC,
		elapsed.Truncate(time.Second),
	)
}

func (s *runtimeStats) report(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Printf("PERF %s", s.snapshot())
		}
	}
}
