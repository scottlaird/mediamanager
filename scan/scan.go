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

// Role says what a file is to its original. Values match catalog.Role.
type Role string

const (
	// Original is a video, audio or still file in its own right.
	Original Role = ""
	// Proxy files live under a Proxy directory and share their original's
	// base name.
	Proxy Role = "proxy"
	// Sidecar files (.sidecar) sit beside the original with the same base.
	Sidecar Role = "sidecar"
	// ProxySidecar files sit beside the proxy.
	ProxySidecar Role = "proxy-sidecar"
)

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
	// Role is Original for media in its own right; companions carry the
	// role that says how they attach to the original with the same Base.
	Role Role
}

// IsOriginal reports whether f is media rather than a companion.
func (f File) IsOriginal() bool { return f.Role == Original }

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
		role := roleOf(rel, kind, ext)
		if kind == media.Unknown && role == Original {
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
			Role:    role,
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

// SidecarExts are the metadata files editors write beside media:
// Blackmagic's .sidecar for BRAW and Adobe's .xmp for stills.
var SidecarExts = map[string]bool{"sidecar": true, "xmp": true}

// IsSidecarExt reports whether a lowercase extension names a sidecar.
func IsSidecarExt(ext string) bool { return SidecarExts[strings.ToLower(ext)] }

// roleOf classifies a file by where it sits and what it is: anything in a
// Proxy directory is a proxy (or a proxy's sidecar), a sidecar extension
// elsewhere is the original's sidecar, and everything else is an original.
func roleOf(rel string, kind media.Kind, ext string) Role {
	inProxy := strings.EqualFold(path.Base(path.Dir(rel)), "proxy")
	switch {
	case IsSidecarExt(ext) && inProxy:
		return ProxySidecar
	case IsSidecarExt(ext):
		return Sidecar
	case inProxy && kind == media.Video:
		return Proxy
	}
	return Original
}

// OriginalDir returns the directory the file's original lives in: the
// file's own directory, or its parent for anything under Proxy/.
func (f File) OriginalDir() string {
	if f.Role == Proxy || f.Role == ProxySidecar {
		return path.Dir(path.Dir(f.Rel))
	}
	return path.Dir(f.Rel)
}
