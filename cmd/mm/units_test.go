package main

import (
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		err  bool
	}{
		{"2T", 2 << 40, false},
		{"500G", 500 << 30, false},
		{"1.5TB", 1.5 * (1 << 40), false},
		{"100MiB", 100 << 20, false},
		{"4096", 4096, false},
		{" 3 g ", 3 << 30, false},
		{"", 0, true},
		{"-1G", 0, true},
		{"lots", 0, true},
	}
	for _, tt := range tests {
		got, err := parseSize(tt.in)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("parseSize(%q) = %d, %v; want %d, err=%v", tt.in, got, err, tt.want, tt.err)
		}
	}
}

func TestParseAge(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"30d", 30 * 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"12h", 12 * time.Hour, false},
		{"1.5d", 36 * time.Hour, false},
		{"soon", 0, true},
		{"-1d", 0, true},
	}
	for _, tt := range tests {
		got, err := parseAge(tt.in)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("parseAge(%q) = %v, %v; want %v, err=%v", tt.in, got, err, tt.want, tt.err)
		}
	}
}

func TestHumanSize(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 3 << 20: "3.0 MiB", 1536 << 30: "1.5 TiB",
	} {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}
