// Package naming turns an original's metadata into its permanent path.
//
// A Scheme decides the directory layout; FileName decides the basename and
// is shared by every scheme so that identity stays recoverable from the
// filesystem alone (rule R7). Output names are lowercase except for the
// ProxyDir directory, which keeps the capitalisation cameras and editors
// expect.
package naming

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/scottlaird/mediamanager/identity"
	"github.com/scottlaird/mediamanager/media"
)

// ProxyDir is the directory, alongside originals, that holds their proxies.
const ProxyDir = "Proxy"

// Asset is what a Scheme needs to know to place an original.
type Asset struct {
	Kind media.Kind
	// OrigName is the basename the file had on its source, including
	// extension, in whatever case the camera wrote it.
	OrigName string
	// ID is the sparse identity. Required for kinds where UsesSparseID is
	// true and ignored otherwise.
	ID identity.ID
	// CaptureTime is when the file was recorded, already in the timezone
	// the layout should use.
	CaptureTime time.Time
}

// Scheme maps an asset to a slash-separated path relative to the root of
// its kind's tree. Implementations must be pure: the same Asset always
// yields the same path.
type Scheme interface {
	Path(a Asset) (string, error)
}

// DateScheme lays files out as YYYY/MM/DD/name. It needs no human input,
// which makes it the default for unattended imports.
type DateScheme struct{}

func (DateScheme) Path(a Asset) (string, error) {
	name, err := FileName(a)
	if err != nil {
		return "", err
	}
	t := a.CaptureTime
	return path.Join(t.Format("2006"), t.Format("01"), t.Format("02"), name), nil
}

// ProjectScheme lays files out as YYYY/YYYYMMDD-project/name, the layout
// used for hand-named shoots.
type ProjectScheme struct {
	Project string
}

func (s ProjectScheme) Path(a Asset) (string, error) {
	name, err := FileName(a)
	if err != nil {
		return "", err
	}
	proj := Sanitize(strings.ReplaceAll(strings.TrimSpace(s.Project), " ", "-"))
	if proj == "" {
		return "", errors.New("naming: empty project name")
	}
	t := a.CaptureTime
	return path.Join(t.Format("2006"), t.Format("20060102")+"-"+proj, name), nil
}

// FileName returns the permanent basename for a: base-id.ext for kinds
// identified by sparse checksum, base.ext otherwise, all lowercase. A source
// name that already carries the same ID (a file copied verbatim from a spool)
// is not given a second suffix.
func FileName(a Asset) (string, error) {
	if a.OrigName == "" {
		return "", errors.New("naming: empty original name")
	}
	if a.CaptureTime.IsZero() {
		return "", fmt.Errorf("naming: %s has no capture time", a.OrigName)
	}
	base, ext := splitExt(a.OrigName)
	base = Sanitize(base)
	ext = Sanitize(ext)
	if base == "" {
		base = "file"
	}
	if !a.Kind.UsesSparseID() {
		return join(base, ext), nil
	}
	if !a.ID.Valid() {
		return "", fmt.Errorf("naming: %s needs a valid id, got %q", a.OrigName, a.ID)
	}
	if p, ok := Parse(join(base, ext)); ok && p.ID == a.ID {
		base = p.Base
	}
	return join(base+"-"+string(a.ID), ext), nil
}

// ProxyPath returns the path of a proxy for the original at rel, keeping
// the original's basename and swapping only the extension.
func ProxyPath(rel, ext string) string {
	dir, name := path.Split(rel)
	base, _ := splitExt(name)
	return path.Join(dir, ProxyDir, join(base, Sanitize(ext)))
}

// SidecarExt is the extension of Blackmagic's per-clip metadata file.
const SidecarExt = "sidecar"

// SidecarPath returns the path of the sidecar beside the original at rel:
// the same name with the extension replaced.
func SidecarPath(rel string) string {
	dir, name := path.Split(rel)
	base, _ := splitExt(name)
	return path.Join(dir, join(base, SidecarExt))
}

// ProxySidecarPath returns the path of the sidecar beside the proxy of rel.
func ProxySidecarPath(rel string) string {
	return ProxyPath(rel, SidecarExt)
}

// WithSuffix inserts _n before the extension, for stills whose name is
// already taken by different content: WithSuffix("a/b.dng", 1) is "a/b_1.dng".
func WithSuffix(rel string, n int) string {
	dir, name := path.Split(rel)
	base, ext := splitExt(name)
	return path.Join(dir, join(fmt.Sprintf("%s_%d", base, n), ext))
}

// Parsed is a permanent filename taken apart.
type Parsed struct {
	Base string
	ID   identity.ID
	Ext  string
}

// Parse recognises a basename of the form base-id.ext produced by FileName
// for a sparse-identified kind. ok is false when the name carries no ID,
// which is how legacy files and stills look.
func Parse(name string) (p Parsed, ok bool) {
	base, ext := splitExt(name)
	i := strings.LastIndexByte(base, '-')
	if i < 1 || len(base)-i-1 != identity.IDLen {
		return Parsed{}, false
	}
	id := identity.ID(base[i+1:])
	if !id.Valid() {
		return Parsed{}, false
	}
	return Parsed{Base: base[:i], ID: id, Ext: ext}, true
}

// Sanitize lowercases s and replaces anything outside [a-z0-9._-] with an
// underscore, so names are safe on every filesystem in play.
func Sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// splitExt returns the name without its last extension, and that extension
// without its dot. A leading dot is not an extension.
func splitExt(name string) (base, ext string) {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 {
		return name, ""
	}
	return name[:i], name[i+1:]
}

func join(base, ext string) string {
	if ext == "" {
		return base
	}
	return base + "." + ext
}
