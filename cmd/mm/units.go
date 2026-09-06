package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseSize reads "2T", "500G", "1.5TB", "100MiB" or a bare byte count.
// Suffixes are binary (1T = 2^40), which is what disks report in practice.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I")
	mult := int64(1)
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		case 'P':
			mult = 1 << 50
		}
		if mult != 1 {
			s = s[:n-1]
		}
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return int64(v * float64(mult)), nil
}

// parseAge reads "30d", "12h", "2w" or any time.Duration string.
func parseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n := len(s); n > 1 {
		mult := time.Duration(0)
		switch s[n-1] {
		case 'd':
			mult = 24 * time.Hour
		case 'w':
			mult = 7 * 24 * time.Hour
		}
		if mult != 0 {
			v, err := strconv.ParseFloat(s[:n-1], 64)
			if err != nil || v < 0 {
				return 0, fmt.Errorf("bad age %q", s)
			}
			return time.Duration(v * float64(mult)), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad age %q", s)
	}
	return d, nil
}

func humanSize(n int64) string {
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
