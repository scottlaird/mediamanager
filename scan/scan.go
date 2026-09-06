// Package scan finds media files on a mounted source and works out what
// they are and when they were shot.
//
// A source is any directory. Two shapes are recognised: DCIM cards from
// still-and-video cameras, and flat piles of clips from cinema cameras.
// Shape only decides where the walk starts; every file is routed by its
// own kind.
package scan

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/scottlaird/mediamanager/media"
)

// Shape is how a source is laid out.
type Shape int

const (
	// Flat sources hold clips anywhere below the root, with proxies in a
	// Proxy directory beside their originals.
	Flat Shape = iota
	// DCIM sources keep everything under DCIM/, as stills cameras do.
	DCIM
)

func (s Shape) String() string {
	if s == DCIM {
		return "dcim"
	}
	return "flat"
}

// File is one media file found on a source.
type File struct {
	// Rel is the slash-separated path relative to the source root.
	Rel string
	// Abs is the path to open.
	Abs     string
	Kind    media.Kind
	Ext     string
	Size    int64
	ModTime time.Time
	// Proxy is set when the file lives under a Proxy directory. Its
	// original is the file in the parent directory with the same Base.
	Proxy bool
}

// Base is the filename without directory or extension, which is what
// pairs a proxy with its original.
func (f File) Base() string {
	name := path.Base(f.Rel)
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

// Name is the basename as the camera wrote it.
func (f File) Name() string { return path.Base(f.Rel) }

// Result is what Scan found.
type Result struct {
	Shape Shape
	// Files is sorted by Rel.
	Files []File
	// Unrecognised lists non-junk files whose extension matched no kind,
	// for the report; nothing is done with them.
	Unrecognised []string
}

// DetectShape reports DCIM when the root has a DCIM directory, matched
// case-insensitively because FAT cards do not promise case.
func DetectShape(root string) Shape {
	if dcimDir(root) != "" {
		return DCIM
	}
	return Flat
}

func dcimDir(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() && strings.EqualFold(e.Name(), "DCIM") {
			return filepath.Join(root, e.Name())
		}
	}
	return ""
}

// Scan walks root and returns every classified media file. Junk is skipped
// silently; symlinks are not followed; a read error anywhere stops the
// scan, because an unreadable card is something to report, not work around.
func Scan(root string, c *Classifier) (Result, error) {
	root = filepath.Clean(root)
	res := Result{Shape: DetectShape(root)}
	start := root
	if res.Shape == DCIM {
		start = dcimDir(root)
	}
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if p != start && IsJunk(name) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		kind, ext := c.Classify(name)
		if kind == media.Unknown {
			res.Unrecognised = append(res.Unrecognised, rel)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		res.Files = append(res.Files, File{
			Rel:     rel,
			Abs:     p,
			Kind:    kind,
			Ext:     ext,
			Size:    info.Size(),
			ModTime: info.ModTime(),
			Proxy:   inProxyDir(rel),
		})
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].Rel < res.Files[j].Rel })
	sort.Strings(res.Unrecognised)
	return res, nil
}

// inProxyDir reports whether the file's immediate directory is a proxy
// directory, in any case.
func inProxyDir(rel string) bool {
	return strings.EqualFold(path.Base(path.Dir(rel)), "proxy")
}

// OriginalDir returns the directory a proxy's original lives in.
func (f File) OriginalDir() string {
	if !f.Proxy {
		return path.Dir(f.Rel)
	}
	return path.Dir(path.Dir(f.Rel))
}
