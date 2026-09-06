package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

// reference is the sparse algorithm written out longhand so the tests check
// the definition rather than the implementation against itself.
func reference(b []byte) ID {
	h := sha256.New()
	var sz [8]byte
	binary.BigEndian.PutUint64(sz[:], uint64(len(b)))
	h.Write(sz[:])
	if len(b) <= 2*Chunk {
		h.Write(b)
	} else {
		h.Write(b[:Chunk])
		h.Write(b[len(b)-Chunk:])
	}
	return ID(hex.EncodeToString(h.Sum(nil))[:IDLen])
}

func TestSparseMatchesDefinition(t *testing.T) {
	for _, n := range []int{0, 1, 4096, Chunk, 2*Chunk - 1, 2 * Chunk, 2*Chunk + 1, 5 * Chunk} {
		b := randBytes(t, n)
		got, err := Sparse(bytes.NewReader(b), int64(n))
		if err != nil {
			t.Fatalf("size %d: %v", n, err)
		}
		if want := reference(b); got != want {
			t.Errorf("size %d: got %s, want %s", n, got, want)
		}
		if !got.Valid() {
			t.Errorf("size %d: id %q not valid", n, got)
		}
	}
}

func TestSparseDetectsTruncation(t *testing.T) {
	b := randBytes(t, 5*Chunk)
	full, _ := Sparse(bytes.NewReader(b), int64(len(b)))
	for _, cut := range []int{len(b) - 1, len(b) - Chunk, len(b) / 2, 2 * Chunk, 1} {
		trunc := b[:cut]
		got, err := Sparse(bytes.NewReader(trunc), int64(len(trunc)))
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if got == full {
			t.Errorf("cut %d: truncated copy has same id %s", cut, got)
		}
	}
}

func TestSparseShortRead(t *testing.T) {
	b := randBytes(t, 3*Chunk)
	_, err := Sparse(bytes.NewReader(b[:len(b)-10]), int64(len(b)))
	if !errors.Is(err, ErrShortRead) {
		t.Fatalf("got %v, want ErrShortRead", err)
	}
}

func TestSparseIgnoresMiddleByDesign(t *testing.T) {
	// The documented trade-off: a change strictly between the head and tail
	// extents is invisible to Sparse and visible to Full.
	b := randBytes(t, 5*Chunk)
	before, _ := Sparse(bytes.NewReader(b), int64(len(b)))
	fullBefore, _ := Full(bytes.NewReader(b))
	b[len(b)/2] ^= 0xff
	after, _ := Sparse(bytes.NewReader(b), int64(len(b)))
	fullAfter, _ := Full(bytes.NewReader(b))
	if before != after {
		t.Errorf("sparse id changed on a middle edit: %s -> %s", before, after)
	}
	if fullBefore == fullAfter {
		t.Errorf("full hash did not change on a middle edit")
	}
}

func TestSparseHeadAndTailEditsDetected(t *testing.T) {
	b := randBytes(t, 5*Chunk)
	base, _ := Sparse(bytes.NewReader(b), int64(len(b)))
	for _, off := range []int{0, Chunk - 1, len(b) - Chunk, len(b) - 1} {
		c := append([]byte(nil), b...)
		c[off] ^= 0xff
		got, _ := Sparse(bytes.NewReader(c), int64(len(c)))
		if got == base {
			t.Errorf("edit at %d not detected", off)
		}
	}
}

func TestSparseFileAndFullFile(t *testing.T) {
	b := randBytes(t, 3*Chunk)
	p := filepath.Join(t.TempDir(), "clip.braw")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	id, size, err := SparseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(b)) {
		t.Errorf("size got %d, want %d", size, len(b))
	}
	if want := reference(b); id != want {
		t.Errorf("id got %s, want %s", id, want)
	}
	full, err := FullFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if want := hex.EncodeToString(sum[:]); full != want {
		t.Errorf("full got %s, want %s", full, want)
	}
	h := NewFullHasher()
	h.Write(b)
	if got := FormatFull(h); got != full {
		t.Errorf("incremental full got %s, want %s", got, full)
	}
}

func TestIDValid(t *testing.T) {
	tests := []struct {
		id   ID
		want bool
	}{
		{"3f9a1c2e7b4d5a60", true},
		{"3F9A1C2E7B4D5A60", false},
		{"3f9a1c2e7b4d5a6", false},
		{"3f9a1c2e7b4d5a600", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := tt.id.Valid(); got != tt.want {
			t.Errorf("%q.Valid() = %v, want %v", tt.id, got, tt.want)
		}
	}
}
