// Package linktree maintains the tree of symlinks the editor sees.
//
// The tree is a derived view: for every asset, one symlink at the asset's
// permanent relative path, pointing at the best complete copy that exists
// right now. Reconcile computes the minimum set of changes to get there and
// applies each with an atomic rename, so a path is never missing while it
// is being repointed. Audit enforces rule R5: nothing but symlinks and
// directories may live here, and a real file is a stop-everything problem
// rather than something to tidy.
package linktree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/scottlaird/mediamanager/catalog"
	"github.com/scottlaird/mediamanager/scan"
)

// tmpPrefix marks the short-lived symlink a swap goes through.
const tmpPrefix = ".mm-tmp-"

// ErrRealFiles is returned by Audit when the tree holds anything other than
// symlinks and directories.
var ErrRealFiles = errors.New("linktree: regular files found in link tree")

// Audit walks root and returns the relative paths of every entry that is
// not a symlink or directory, ignoring platform junk. A non-empty result
// comes with ErrRealFiles. A root that does not exist yet is fine.
func Audit(root string) ([]string, error) {
	var bad []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return err
		}
		name := d.Name()
		if p != root && (scan.IsJunk(name) || strings.HasPrefix(name, tmpPrefix)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		bad = append(bad, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(bad) > 0 {
		return bad, ErrRealFiles
	}
	return nil, nil
}

// Report says what Reconcile did.
type Report struct {
	Created   []string
	Updated   []string
	Unchanged int
	// Unknown lists symlinks in the tree that no asset wanted. They are
	// left alone unless Options.Prune is set, in which case they are
	// removed and listed here anyway.
	Unknown []string
}

// Options tune Reconcile.
type Options struct {
	// Prune removes symlinks not present in want. Off by default: a
	// symlink is harmless and might be the user's.
	Prune bool
}

// Reconcile makes root contain a symlink at every key of want pointing at
// its value (an absolute path), creating directories as needed. Existing
// symlinks with the right target are untouched; wrong ones are replaced
// atomically. A regular file or directory where a symlink should be is an
// error and is never replaced.
func Reconcile(root string, want map[string]string, opts Options) (Report, error) {
	var rep Report
	if err := os.MkdirAll(root, 0o755); err != nil {
		return rep, err
	}
	rels := make([]string, 0, len(want))
	for rel := range want {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		target := want[rel]
		if !filepath.IsAbs(target) {
			return rep, fmt.Errorf("linktree: target for %s is not absolute: %s", rel, target)
		}
		link := filepath.Join(root, filepath.FromSlash(rel))
		have, err := os.Readlink(link)
		switch {
		case err == nil && have == target:
			rep.Unchanged++
			continue
		case err == nil:
			if err := swap(link, target); err != nil {
				return rep, err
			}
			rep.Updated = append(rep.Updated, rel)
		case errors.Is(err, fs.ErrNotExist):
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				return rep, err
			}
			if err := swap(link, target); err != nil {
				return rep, err
			}
			rep.Created = append(rep.Created, rel)
		default:
			// Readlink fails with EINVAL on anything that is not a symlink.
			return rep, fmt.Errorf("linktree: %s exists and is not a symlink; refusing to replace it", link)
		}
	}
	unknown, err := extraLinks(root, want)
	if err != nil {
		return rep, err
	}
	rep.Unknown = unknown
	if opts.Prune {
		for _, rel := range unknown {
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
				return rep, err
			}
		}
	}
	return rep, nil
}

// swap installs a symlink at link via a temporary name and rename, so link
// always resolves to either the old target or the new one.
func swap(link, target string) error {
	tmp := filepath.Join(filepath.Dir(link), tmpPrefix+filepath.Base(link))
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func extraLinks(root string, want map[string]string) ([]string, error) {
	var extra []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if p != root && scan.IsJunk(name) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, tmpPrefix) {
			_ = os.Remove(p) // leftover from an interrupted swap
			return nil
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if _, ok := want[rel]; !ok {
			extra = append(extra, rel)
		}
		return nil
	})
	sort.Strings(extra)
	return extra, err
}

// Dangling lists symlinks under root whose target does not currently
// exist: assets whose only copy is on an unplugged card, or a spool that
// is not mounted right now.
func Dangling(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return err
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		if _, err := os.Stat(p); err != nil {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// Choose picks the copy a link should point at: the first Complete entry
// of copies (which catalog.Copies already orders spool, NAS, source) whose
// location has a root right now. rootOf returns false for a location that
// is not mounted. ok is false when nothing usable exists.
func Choose(copies []catalog.Copy, rootOf func(catalog.Location) (string, bool)) (target string, ok bool) {
	for _, cp := range copies {
		if cp.State != catalog.Complete {
			continue
		}
		root, mounted := rootOf(cp.Location)
		if !mounted {
			continue
		}
		return filepath.Join(root, filepath.FromSlash(cp.RelPath)), true
	}
	return "", false
}
