package main

import (
	"strings"
	"testing"
	"time"
)

func TestRuntimeStatsSnapshot(t *testing.T) {
	stats := newRuntimeStats()
	stats.lines.Add(10)
	stats.detections.Add(2)
	stats.parseErrors.Add(1)
	stats.queueDrops.Add(3)
	stats.parseNanos.Add(int64(250 * time.Microsecond))

	out := stats.snapshot()
	for _, want := range []string{
		"lines=10",
		"detections=2",
		"parse_errors=1",
		"queue_drops=3",
		"avg_parse=25",
		"goroutines=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot %q does not contain %q", out, want)
		}
	}
}

func TestRuntimeStatsEmptySnapshot(t *testing.T) {
	out := newRuntimeStats().snapshot()
	if !strings.Contains(out, "lines=0") || !strings.Contains(out, "avg_parse=0s") {
		t.Errorf("empty snapshot = %q", out)
	}
}
