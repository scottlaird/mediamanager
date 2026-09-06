package ingest

import (
	"fmt"
	"time"
)

// progress turns byte counts from the copy primitive into log lines with
// the current and average rate and a time-to-go. The copy reports every
// few tens of MiB, which at NAS speeds is many times a second, so lines
// are throttled to one every logEvery plus the final one.
type progress struct {
	logf  func(string, ...any)
	label string
	start time.Time
	last  time.Time
	lastN int64
	from  int64
}

const logEvery = 5 * time.Second

func newProgress(logf func(string, ...any), label string, resumedFrom int64) *progress {
	now := time.Now()
	return &progress{logf: logf, label: label, start: now, last: now, lastN: resumedFrom, from: resumedFrom}
}

func (p *progress) report(done, total int64) {
	if p.logf == nil {
		return
	}
	now := time.Now()
	final := done >= total
	if !final && now.Sub(p.last) < logEvery {
		return
	}
	cur := rate(done-p.lastN, now.Sub(p.last))
	avg := rate(done-p.from, now.Sub(p.start))
	eta := ""
	if !final && avg > 0 {
		left := time.Duration(float64(total-done)/avg) * time.Second
		eta = ", " + left.Round(time.Second).String() + " left"
	}
	p.logf("  %s: %d%% %s/%s at %s/s (avg %s/s%s)", p.label, done*100/max(total, 1),
		fmtBytes(done), fmtBytes(total), fmtBytes(int64(cur)), fmtBytes(int64(avg)), eta)
	p.last, p.lastN = now, done
}

func rate(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) / d.Seconds()
}

func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
