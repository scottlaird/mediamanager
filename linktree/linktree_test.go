package linktree

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/scottlaird/mediamanager/catalog"
)

func touch(t *testing.T, p string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readlink(t *testing.T, p string) string {
	t.Helper()
	s, err := os.Readlink(p)
	if err != nil {
		t.Fatalf("readlink %s: %v", p, err)
	}
	return s
}

func TestReconcile(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "links")
	spool := filepath.Join(base, "spool")
	nas := filepath.Join(base, "nas")
	a := "2026/09/05/a-3f9a1c2e7b4d5a60.braw"
	b := "2026/09/05/b-0000000000000001.braw"
	p := "2026/09/05/Proxy/a-3f9a1c2e7b4d5a60.mp4"
	for _, f := range []string{a, b, p} {
		touch(t, filepath.Join(spool, f))
		touch(t, filepath.Join(nas, f))
	}

	want := map[string]string{
		a: filepath.Join(spool, a),
		b: filepath.Join(spool, b),
		p: filepath.Join(spool, p),
	}
	rep, err := Reconcile(root, want, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Created) != 3 || len(rep.Updated) != 0 || rep.Unchanged != 0 || len(rep.Unknown) != 0 {
		t.Errorf("first run: %+v", rep)
	}
	for rel, target := range want {
		if got := readlink(t, filepath.Join(root, rel)); got != target {
			t.Errorf("%s -> %s, want %s", rel, got, target)
		}
	}

	// Second run is a no-op.
	rep, err = Reconcile(root, want, Options{})
	if err != nil || rep.Unchanged != 3 || len(rep.Created)+len(rep.Updated) != 0 {
		t.Errorf("second run: %+v, %v", rep, err)
	}

	// Flush a from the spool: its link moves to the NAS, nothing else changes.
	want[a] = filepath.Join(nas, a)
	rep, err = Reconcile(root, want, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.Updated, []string{a}) || rep.Unchanged != 2 {
		t.Errorf("after flush: %+v", rep)
	}
	if got := readlink(t, filepath.Join(root, a)); got != filepath.Join(nas, a) {
		t.Errorf("a -> %s", got)
	}
	// The link tree still holds only symlinks and dirs, and no temp files.
	if bad, err := Audit(root); err != nil || bad != nil {
		t.Errorf("audit after reconcile: %v, %v", bad, err)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "2026/09/05", tmpPrefix+"*"))
	if len(matches) != 0 {
		t.Errorf("temp links left: %v", matches)
	}
}

func TestReconcileUnknownAndPrune(t *testing.T) {
	root := t.TempDir()
	stray := filepath.Join(root, "2026/stray.braw")
	os.MkdirAll(filepath.Dir(stray), 0o755)
	os.Symlink("/nowhere", stray)
	os.WriteFile(filepath.Join(root, ".DS_Store"), nil, 0o644)

	rep, err := Reconcile(root, map[string]string{"2026/a.braw": "/tmp/a"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.Unknown, []string{"2026/stray.braw"}) {
		t.Errorf("unknown = %v", rep.Unknown)
	}
	if _, err := os.Lstat(stray); err != nil {
		t.Error("unknown link removed without Prune")
	}
	rep, err = Reconcile(root, map[string]string{"2026/a.braw": "/tmp/a"}, Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stray); !errors.Is(err, os.ErrNotExist) {
		t.Error("unknown link survived Prune")
	}
	if len(rep.Unknown) != 1 {
		t.Errorf("pruned links not reported: %+v", rep)
	}
}

func TestReconcileRefusesToReplaceRealFile(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "2026/a.braw")
	touch(t, real)
	_, err := Reconcile(root, map[string]string{"2026/a.braw": "/tmp/a"}, Options{})
	if err == nil {
		t.Fatal("replaced a regular file with a symlink")
	}
	if fi, _ := os.Lstat(real); fi == nil || !fi.Mode().IsRegular() {
		t.Error("regular file was disturbed")
	}
	if _, err := Reconcile(root, map[string]string{"b": "relative/target"}, Options{}); err == nil {
		t.Error("relative target accepted")
	}
}

func TestAudit(t *testing.T) {
	root := t.TempDir()
	if bad, err := Audit(filepath.Join(root, "missing")); err != nil || bad != nil {
		t.Errorf("missing root: %v, %v", bad, err)
	}
	os.MkdirAll(filepath.Join(root, "2026/09/05/Proxy"), 0o755)
	os.Symlink("/tmp/x", filepath.Join(root, "2026/09/05/ok.braw"))
	os.WriteFile(filepath.Join(root, "2026/.DS_Store"), nil, 0o644)
	os.WriteFile(filepath.Join(root, "2026/09/._ok.braw"), nil, 0o644)
	os.WriteFile(filepath.Join(root, "2026/09/05", tmpPrefix+"leftover"), nil, 0o644)
	if bad, err := Audit(root); err != nil || bad != nil {
		t.Errorf("clean tree: %v, %v", bad, err)
	}

	touch(t, filepath.Join(root, "2026/09/05/real.braw"))
	touch(t, filepath.Join(root, "2026/09/05/Proxy/real.mp4"))
	bad, err := Audit(root)
	if !errors.Is(err, ErrRealFiles) {
		t.Fatalf("err = %v, want ErrRealFiles", err)
	}
	want := []string{"2026/09/05/Proxy/real.mp4", "2026/09/05/real.braw"}
	if !reflect.DeepEqual(bad, want) {
		t.Errorf("bad = %v, want %v", bad, want)
	}
}

func TestDangling(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "links")
	target := filepath.Join(base, "present")
	touch(t, target)
	os.MkdirAll(filepath.Join(root, "d"), 0o755)
	os.Symlink(target, filepath.Join(root, "d/ok"))
	os.Symlink(filepath.Join(base, "card", "gone.braw"), filepath.Join(root, "d/gone"))
	got, err := Dangling(root)
	if err != nil || !reflect.DeepEqual(got, []string{"d/gone"}) {
		t.Errorf("Dangling = %v, %v", got, err)
	}
	if got, err := Dangling(filepath.Join(base, "nope")); err != nil || got != nil {
		t.Errorf("missing root: %v, %v", got, err)
	}
}

func TestChoose(t *testing.T) {
	loc := func(id int64, name string) catalog.Location {
		return catalog.Location{ID: id, Name: name}
	}
	copies := []catalog.Copy{
		{RelPath: "video/a.braw", State: catalog.Partial, Location: loc(1, "fast")},
		{RelPath: "video/a.braw", State: catalog.Complete, Location: loc(2, "slow")},
		{RelPath: "video/a.braw", State: catalog.Complete, Location: loc(3, "nas")},
		{RelPath: "DCIM/A.BRAW", State: catalog.Complete, Location: loc(4, "card")},
	}
	roots := map[string]string{"fast": "/Volumes/Fast", "slow": "/Volumes/Slow", "nas": "/Volumes/nas", "card": "/Volumes/CARD"}
	rootOf := func(l catalog.Location) (string, bool) {
		r, ok := roots[l.Name]
		return r, ok
	}
	if got, ok := Choose(copies, rootOf); !ok || got != "/Volumes/Slow/video/a.braw" {
		t.Errorf("all mounted: %q, %v", got, ok)
	}
	delete(roots, "slow")
	if got, ok := Choose(copies, rootOf); !ok || got != "/Volumes/nas/video/a.braw" {
		t.Errorf("slow unmounted: %q, %v", got, ok)
	}
	delete(roots, "nas")
	if got, ok := Choose(copies, rootOf); !ok || got != "/Volumes/CARD/DCIM/A.BRAW" {
		t.Errorf("only card: %q, %v", got, ok)
	}
	delete(roots, "card")
	if _, ok := Choose(copies, rootOf); ok {
		t.Error("chose a partial or unmounted copy")
	}
}
