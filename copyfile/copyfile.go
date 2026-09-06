// Package copyfile is the one place mediamanager moves bytes.
//
// Copy writes to dst+".partial", fsyncs, verifies the destination against
// the source's sparse identity, restores the source mtime, and only then
// renames into place. A file with its final name is therefore complete by
// construction (rule R2), and a completed destination is never overwritten
// (R3). Interrupted copies resume from the partial after checking that its
// tail still matches the source, so retrying is always safe and usually
// cheap.
package copyfile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/scottlaird/mediamanager/identity"
)

// PartialSuffix marks an in-progress destination.
const PartialSuffix = ".partial"

var (
	// ErrExists means dst already exists as a completed file with different
	// content. Copy never overwrites it.
	ErrExists = errors.New("copyfile: destination exists with different content")
	// ErrSourceMismatch means the source is not the file the caller expected.
	ErrSourceMismatch = errors.New("copyfile: source does not match expected identity")
	// ErrVerify means the destination did not match the source after copying.
	// The partial is left in place for inspection and a retry restarts it.
	ErrVerify = errors.New("copyfile: destination failed verification")
)

// Options tune a Copy. The zero value is usable.
type Options struct {
	// ExpectID, when set, is what the source's sparse identity must be. A
	// mismatch fails before any bytes move.
	ExpectID identity.ID
	// Progress, when set, is called with bytes copied so far and the total
	// after roughly every ProgressEvery bytes, and once at the end. It runs
	// on the copying goroutine; keep it quick.
	Progress func(copied, total int64)
	// ProgressEvery defaults to 64 MiB.
	ProgressEvery int64
	// BufferSize defaults to 4 MiB.
	BufferSize int
}

// Result describes a finished Copy.
type Result struct {
	Size int64
	// ID is the sparse identity, verified on the destination.
	ID identity.ID
	// FullSHA256 covers every byte of the destination, including any prefix
	// reused from a partial (that prefix is re-read locally to hash it).
	FullSHA256 string
	// Resumed is how many bytes were kept from an existing partial.
	Resumed int64
	// AlreadyComplete is set when dst already existed and verified, in which
	// case FullSHA256 is empty because nothing was read in full.
	AlreadyComplete bool
}

// Copy copies src to dst as described in the package comment. It is
// idempotent: a completed dst verifies and returns immediately, an
// interrupted one resumes. Cancelling ctx leaves the partial for a later
// resume and returns ctx.Err().
func Copy(ctx context.Context, src, dst string, opts Options) (Result, error) {
	opts = opts.withDefaults()

	in, err := os.Open(src)
	if err != nil {
		return Result{}, err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return Result{}, err
	}
	size := st.Size()

	srcID, err := identity.Sparse(in, size)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", src, err)
	}
	if opts.ExpectID != "" && srcID != opts.ExpectID {
		return Result{}, fmt.Errorf("%w: %s is %s, want %s", ErrSourceMismatch, src, srcID, opts.ExpectID)
	}

	if done, err := completed(dst, size, srcID); err != nil || done {
		if err != nil {
			return Result{}, err
		}
		return Result{Size: size, ID: srcID, AlreadyComplete: true}, nil
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Result{}, err
	}
	partial := dst + PartialSuffix
	out, err := os.OpenFile(partial, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return Result{}, err
	}
	defer out.Close()

	h := identity.NewFullHasher()
	resumed, err := resumePoint(in, out, size, h)
	if err != nil {
		return Result{}, err
	}

	if err := stream(ctx, in, out, resumed, size, h, opts); err != nil {
		return Result{}, err
	}
	if err := out.Sync(); err != nil {
		return Result{}, err
	}

	dstID, err := verify(out, size, srcID)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %s is %s, source is %s", ErrVerify, partial, dstID, srcID)
	}
	if err := os.Chtimes(partial, st.ModTime(), st.ModTime()); err != nil {
		return Result{}, err
	}
	if err := out.Close(); err != nil {
		return Result{}, err
	}
	if err := os.Rename(partial, dst); err != nil {
		return Result{}, err
	}
	syncDir(filepath.Dir(dst))

	return Result{
		Size:       size,
		ID:         srcID,
		FullSHA256: identity.FormatFull(h),
		Resumed:    resumed,
	}, nil
}

func (o Options) withDefaults() Options {
	if o.ProgressEvery <= 0 {
		o.ProgressEvery = 64 << 20
	}
	if o.BufferSize <= 0 {
		o.BufferSize = 4 << 20
	}
	return o
}

// completed reports whether dst already exists as a complete copy. A dst
// that exists but does not match is ErrExists: the caller has a naming
// collision or a corrupted file, and either way it is not ours to replace.
func completed(dst string, size int64, want identity.ID) (bool, error) {
	f, err := os.Open(dst)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := verify(f, size, want); err != nil {
		return false, fmt.Errorf("%w: %s", ErrExists, dst)
	}
	return true, nil
}

// verify checks that f has the expected size and sparse identity and
// returns the identity it found.
func verify(f *os.File, size int64, want identity.ID) (identity.ID, error) {
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.Size() != size {
		return "", fmt.Errorf("size %d, want %d", st.Size(), size)
	}
	got, err := identity.Sparse(f, size)
	if err != nil {
		return "", err
	}
	if got != want {
		return got, errors.New("identity mismatch")
	}
	return got, nil
}

// resumePoint decides how much of an existing partial to keep. The kept
// prefix is re-read into h so the full hash still covers the whole file.
// Anything doubtful is thrown away: a partial longer than the source, or one
// whose final bytes differ from the source at the same offset.
func resumePoint(in, out *os.File, size int64, h io.Writer) (int64, error) {
	st, err := out.Stat()
	if err != nil {
		return 0, err
	}
	n := st.Size()
	if n == 0 || n > size || !tailMatches(in, out, n) {
		if err := out.Truncate(0); err != nil {
			return 0, err
		}
		return 0, nil
	}
	if _, err := io.Copy(h, io.NewSectionReader(out, 0, n)); err != nil {
		return 0, err
	}
	return n, nil
}

func tailMatches(in, out *os.File, n int64) bool {
	l := min(n, int64(identity.Chunk))
	a := make([]byte, l)
	b := make([]byte, l)
	if _, err := out.ReadAt(a, n-l); err != nil {
		return false
	}
	if _, err := in.ReadAt(b, n-l); err != nil {
		return false
	}
	return bytes.Equal(a, b)
}

// stream copies bytes [from, size) of in to the same offsets of out, hashing
// as it goes, honouring ctx between buffers.
func stream(ctx context.Context, in, out *os.File, from, size int64, h io.Writer, opts Options) error {
	if _, err := in.Seek(from, io.SeekStart); err != nil {
		return err
	}
	if _, err := out.Seek(from, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, opts.BufferSize)
	w := io.MultiWriter(out, h)
	copied := from
	lastReport := from
	for copied < size {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := min(int64(len(buf)), size-copied)
		n, err := io.ReadFull(in, buf[:want])
		if err != nil {
			return fmt.Errorf("copyfile: reading %s at %d: %w", in.Name(), copied, err)
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return err
		}
		copied += int64(n)
		if opts.Progress != nil && copied-lastReport >= opts.ProgressEvery {
			opts.Progress(copied, size)
			lastReport = copied
		}
	}
	if opts.Progress != nil && lastReport != copied {
		opts.Progress(copied, size)
	}
	return nil
}

// syncDir makes the rename durable. Failures are ignored: some filesystems
// (notably SMB mounts) refuse to fsync a directory, and durability of the
// directory entry is a nicety here, not an invariant.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
