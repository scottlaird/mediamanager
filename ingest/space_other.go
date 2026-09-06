//go:build !darwin && !linux

package ingest

import "math"

func statfsFree(path string) (int64, error) { return math.MaxInt64, nil }
