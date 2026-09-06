package copyfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scottlaird/mediamanager/identity"
)

const mib = 1 << 20

var stamp = time.Date(2026, 9, 5, 14, 12, 0, 0, time.UTC)

func writeSource(t *testing.T, dir, name string, n int) (string, []byte) {
	t.Helper()
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return p, b
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestCopy(t *testing.T) {
	for _, n := range []int{0, 100, 2 * mib, 5*mib + 17} {
		dir := t.TempDir()
		src, b := writeSource(t, dir, "clip.braw", n)
		dst := filepath.Join(dir, "out", "2026", "clip.braw")

		res, err := Copy(context.Background(), src, dst, Options{})
		if err != nil {
			t.Fatalf("size %d: %v", n, err)
		}
		if got := mustRead(t, dst); !bytes.Equal(got, b) {
			t.Errorf("size %d: content differs", n)
		}
		if res.Size != int64(n) || res.Resumed != 0 || res.AlreadyComplete {
			t.Errorf("size %d: result %+v", n, res)
		}
		if res.FullSHA256 != sha(b) {
			t.Errorf("size %d: full hash got %s, want %s", n, res.FullSHA256, sha(b))
		}
		wantID, _ := identity.Sparse(bytes.NewReader(b), int64(n))
		if res.ID != wantID {
			t.Errorf("size %d: id got %s, want %s", n, res.ID, wantID)
		}
		st, _ := os.Stat(dst)
		if !st.ModTime().Equal(stamp) {
			t.Errorf("size %d: mtime %v, want %v", n, st.ModTime(), stamp)
		}
		if exists(dst + PartialSuffix) {
			t.Errorf("size %d: partial left behind", n)
		}
	}
}

func TestCopyAlreadyComplete(t *testing.T) {
	dir := t.TempDir()
	src, _ := writeSource(t, dir, "clip.braw", 3*mib)
	dst := filepath.Join(dir, "clip.braw.copy")
	if _, err := Copy(context.Background(), src, dst, Options{}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(dst)
	later := stamp.Add(time.Hour)
	os.Chtimes(dst, later, later) // detect any rewrite

	res, err := Copy(context.Background(), src, dst, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.AlreadyComplete || res.Size != before.Size() {
		t.Errorf("result %+v", res)
	}
	after, _ := os.Stat(dst)
	if !after.ModTime().Equal(later) {
		t.Errorf("completed destination was rewritten")
	}
}

func TestCopyRefusesDifferentExisting(t *testing.T) {
	dir := t.TempDir()
	src, _ := writeSource(t, dir, "clip.braw", 3*mib)
	dst := filepath.Join(dir, "other.braw")
	other := []byte("not the same file at all")
	os.WriteFile(dst, other, 0o644)

	_, err := Copy(context.Background(), src, dst, Options{})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
	if got := mustRead(t, dst); !bytes.Equal(got, other) {
		t.Errorf("existing destination was modified")
	}
}

func TestCopyExpectID(t *testing.T) {
	dir := t.TempDir()
	src, _ := writeSource(t, dir, "clip.braw", 3*mib)
	_, err := Copy(context.Background(), src, filepath.Join(dir, "d"), Options{ExpectID: "0000000000000000"})
	if !errors.Is(err, ErrSourceMismatch) {
		t.Fatalf("got %v, want ErrSourceMismatch", err)
	}
	if exists(filepath.Join(dir, "d"+PartialSuffix)) {
		t.Errorf("partial created despite source mismatch")
	}
}

func TestCopyResumes(t *testing.T) {
	tests := []struct {
		name        string
		partial     func(b []byte) []byte
		wantResumed int64
	}{
		{"good prefix", func(b []byte) []byte { return b[:3*mib] }, 3 * mib},
		{"tiny prefix", func(b []byte) []byte { return b[:10] }, 10},
		{"corrupt tail", func(b []byte) []byte {
			p := append([]byte(nil), b[:3*mib]...)
			p[len(p)-1] ^= 0xff
			return p
		}, 0},
		{"corrupt middle of prefix", func(b []byte) []byte {
			// Beyond the tail window, so undetectable at resume time: the
			// prefix is trusted, and the design accepts that.
			p := append([]byte(nil), b[:3*mib]...)
			p[mib] ^= 0xff
			return p
		}, 3 * mib},
		{"longer than source", func(b []byte) []byte { return append(append([]byte(nil), b...), 1, 2, 3) }, 0},
		{"complete partial", func(b []byte) []byte { return b }, int64(5 * mib)},
	}
	for _, tt := range tests {
		dir := t.TempDir()
		src, b := writeSource(t, dir, "clip.braw", 5*mib)
		dst := filepath.Join(dir, "out.braw")
		os.WriteFile(dst+PartialSuffix, tt.partial(b), 0o644)

		res, err := Copy(context.Background(), src, dst, Options{HashResumedPrefix: true})
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if res.Resumed != tt.wantResumed {
			t.Errorf("%s: resumed %d, want %d", tt.name, res.Resumed, tt.wantResumed)
		}
		got := mustRead(t, dst)
		if tt.name == "corrupt middle of prefix" {
			// Documented gap: the result is wrong but the full hash says so.
			if bytes.Equal(got, b) {
				t.Errorf("%s: expected the trusted prefix to carry the corruption", tt.name)
			}
			if res.FullSHA256 == sha(b) {
				t.Errorf("%s: full hash should reveal the corruption", tt.name)
			}
			continue
		}
		if !bytes.Equal(got, b) {
			t.Errorf("%s: content differs", tt.name)
		}
		if res.FullSHA256 != sha(b) {
			t.Errorf("%s: full hash got %s, want %s", tt.name, res.FullSHA256, sha(b))
		}
	}
}

func TestCopyCancelThenResume(t *testing.T) {
	dir := t.TempDir()
	src, b := writeSource(t, dir, "clip.braw", 6*mib)
	dst := filepath.Join(dir, "out.braw")

	ctx, cancel := context.WithCancel(context.Background())
	var reports int
	_, err := Copy(ctx, src, dst, Options{
		BufferSize:    mib,
		ProgressEvery: mib,
		Progress: func(copied, total int64) {
			reports++
			if copied >= 2*mib {
				cancel()
			}
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if exists(dst) {
		t.Fatal("destination exists after cancelled copy")
	}
	st, err := os.Stat(dst + PartialSuffix)
	if err != nil || st.Size() < 2*mib || st.Size() >= 6*mib {
		t.Fatalf("partial after cancel: %v, %v", st, err)
	}

	res, err := Copy(context.Background(), src, dst, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resumed != st.Size() {
		t.Errorf("resumed %d, want %d", res.Resumed, st.Size())
	}
	if got := mustRead(t, dst); !bytes.Equal(got, b) {
		t.Error("content differs after resume")
	}
	if res.FullSHA256 != "" {
		t.Errorf("resumed copy without HashResumedPrefix reported a full hash %s", res.FullSHA256)
	}
}

func TestResumeHashesPrefixOnlyWhenAsked(t *testing.T) {
	dir := t.TempDir()
	src, b := writeSource(t, dir, "clip.braw", 5*mib)
	dst := filepath.Join(dir, "out.braw")
	os.WriteFile(dst+PartialSuffix, b[:3*mib], 0o644)
	res, err := Copy(context.Background(), src, dst, Options{HashResumedPrefix: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resumed != 3*mib || res.FullSHA256 != sha(b) {
		t.Errorf("result %+v", res)
	}
}

func TestCopyProgressReachesTotal(t *testing.T) {
	dir := t.TempDir()
	src, _ := writeSource(t, dir, "clip.braw", 3*mib+5)
	var last, total int64
	_, err := Copy(context.Background(), src, filepath.Join(dir, "o"), Options{
		BufferSize: mib, ProgressEvery: mib,
		Progress: func(c, t int64) { last, total = c, t },
	})
	if err != nil {
		t.Fatal(err)
	}
	if last != total || total != 3*mib+5 {
		t.Errorf("final progress %d/%d", last, total)
	}
}
