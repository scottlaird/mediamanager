package ingest

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var sprintf = fmt.Sprintf

func TestProgressThrottlesAndReportsRate(t *testing.T) {
	var lines []string
	p := newProgress(func(f string, a ...any) { lines = append(lines, strings.TrimSpace(sprintf(f, a...))) }, "clip", 0)
	// Pretend 10 s have passed at 100 MiB/s.
	p.start = p.start.Add(-10 * time.Second)
	p.last = p.start
	p.report(1000<<20, 2000<<20)
	if len(lines) != 1 || !strings.Contains(lines[0], "50%") || !strings.Contains(lines[0], "100.0 MiB/s") || !strings.Contains(lines[0], "left") {
		t.Fatalf("first line = %q", lines)
	}
	// Immediately after, nothing (throttled) unless it is the end.
	p.report(1001<<20, 2000<<20)
	if len(lines) != 1 {
		t.Fatalf("throttle failed: %q", lines)
	}
	p.report(2000<<20, 2000<<20)
	if len(lines) != 2 || !strings.Contains(lines[1], "100%") || strings.Contains(lines[1], "left") {
		t.Fatalf("final line = %q", lines)
	}
	// Silent without a logger.
	newProgress(nil, "x", 0).report(1, 2)
}

func TestFmtBytes(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 1536: "1.5 KiB", 800 << 20: "800.0 MiB", 6 << 40: "6.0 TiB"} {
		if got := fmtBytes(in); got != want {
			t.Errorf("fmtBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
