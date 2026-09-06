// Package identity computes content-derived identities for media files.
//
// Large originals (video, audio) get a sparse identity: a hash over the
// file size and its first and last 1 MiB. It costs two small reads
// regardless of file size, is identical for every true copy of the file,
// and differs for any truncated copy because the size is part of the hash.
// It does not detect corruption in the middle of a file; that trade is
// deliberate and is why every copy the tool performs also records a full
// hash as a side effect of streaming the bytes.
package identity

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"regexp"
)

// Scheme names the sparse algorithm so it can change without invalidating
// stored identities.
const Scheme = "s1"

// Chunk is the size of the head and tail extents hashed by Sparse.
const Chunk = 1 << 20

// IDLen is the number of hex characters in an ID.
const IDLen = 16

// ID is a sparse identity: the first IDLen hex characters of the hash.
type ID string

var idRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Valid reports whether id has the shape of an ID.
func (id ID) Valid() bool { return idRE.MatchString(string(id)) }

// ErrShortRead is returned when the source is smaller than its declared size.
var ErrShortRead = errors.New("identity: short read")

// Sparse computes the sparse identity of a file of the given size.
//
// Files no larger than 2*Chunk are hashed in full, which keeps the identity
// well defined for tiny files and means a small file's sparse identity covers
// every byte.
func Sparse(r io.ReaderAt, size int64) (ID, error) {
	if size < 0 {
		return "", fmt.Errorf("identity: negative size %d", size)
	}
	h := sha256.New()
	var sz [8]byte
	binary.BigEndian.PutUint64(sz[:], uint64(size))
	h.Write(sz[:])

	if size <= 2*Chunk {
		if err := hashRange(h, r, 0, size); err != nil {
			return "", err
		}
	} else {
		if err := hashRange(h, r, 0, Chunk); err != nil {
			return "", err
		}
		if err := hashRange(h, r, size-Chunk, Chunk); err != nil {
			return "", err
		}
	}
	return ID(hex.EncodeToString(h.Sum(nil))[:IDLen]), nil
}

// SparseFile computes the sparse identity of the file at path and returns
// it with the size that was hashed.
func SparseFile(path string) (ID, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	id, err := Sparse(f, st.Size())
	if err != nil {
		return "", 0, fmt.Errorf("%s: %w", path, err)
	}
	return id, st.Size(), nil
}

func hashRange(h hash.Hash, r io.ReaderAt, off, n int64) error {
	if n == 0 {
		return nil
	}
	written, err := io.Copy(h, io.NewSectionReader(r, off, n))
	if err != nil {
		return err
	}
	if written != n {
		return ErrShortRead
	}
	return nil
}

// Full returns the hex SHA-256 of everything read from r. It is the identity
// used for stills and the hash recorded as a side effect of every copy.
func Full(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FullFile returns the hex SHA-256 of the file at path.
func FullFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return Full(f)
}

// NewFullHasher returns a hash suitable for computing a Full identity
// incrementally, for callers that stream bytes somewhere else at the same
// time. Format the result with FormatFull.
func NewFullHasher() hash.Hash { return sha256.New() }

// FormatFull renders a finished NewFullHasher as the same hex string Full
// would return.
func FormatFull(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }
