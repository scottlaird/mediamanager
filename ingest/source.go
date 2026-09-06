package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/scottlaird/mediamanager/catalog"
	"github.com/scottlaird/mediamanager/identity"
	"github.com/scottlaird/mediamanager/media"
	"github.com/scottlaird/mediamanager/naming"
	"github.com/scottlaird/mediamanager/scan"
)

// Source is the outcome of scanning one mounted source.
type Source struct {
	Loc   catalog.Location
	Root  string
	Shape scan.Shape
	// Assets is every asset on the source, new or already known, in scan
	// order. New is the subset first seen this run.
	Assets []string
	New    []string
	// Unrecognised are non-junk files with no known extension; Unrouted
	// are recognised kinds with no tree configured; Orphans are proxies
	// and sidecars whose original was not on the source.
	Unrecognised []string
	Unrouted     []string
	Orphans      []string
}

// ScanSource registers everything on the source at root: each original
// gets an identity, a permanent name and a catalog row, and is recorded as
// a complete copy on the source. Nothing is copied. Re-running on the same
// card is a stat per file thanks to the source-file cache.
func (e *Env) ScanSource(ctx context.Context, root string) (*Source, error) {
	e.init()
	root = filepath.Clean(root)
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("ingest: source %s is not a directory", root)
	}
	loc, err := e.sourceLocation(ctx, root)
	if err != nil {
		return nil, err
	}
	res, err := scan.Scan(root, e.classifier)
	if err != nil {
		return nil, err
	}
	src := &Source{Loc: loc, Root: root, Shape: res.Shape, Unrecognised: res.Unrecognised}
	byKey := map[string][]catalog.Asset{} // originals by dir/base, for companion pairing

	for _, f := range res.Files {
		if !f.IsOriginal() {
			continue
		}
		if _, ok := e.Config.Tree(f.Kind); !ok {
			src.Unrouted = append(src.Unrouted, f.Rel)
			continue
		}
		a, isNew, err := e.register(ctx, loc, f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Rel, err)
		}
		src.Assets = append(src.Assets, a.ID)
		if isNew {
			src.New = append(src.New, a.ID)
		}
		k := path.Join(f.OriginalDir(), f.Base())
		byKey[k] = append(byKey[k], a)
	}
	for _, f := range res.Files {
		if f.IsOriginal() {
			continue
		}
		cands := byKey[path.Join(f.OriginalDir(), f.Base())]
		if f.Role == scan.Proxy {
			cands = videoOnly(cands)
		}
		if len(cands) == 0 {
			src.Orphans = append(src.Orphans, f.Rel)
			continue
		}
		a := preferredOwner(cands, f.Ext)
		sum, err := identity.FullFile(f.Abs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Rel, err)
		}
		cp := catalog.Companion{AssetID: a.ID, LocationID: loc.ID, Role: catalog.Role(f.Role), Ext: f.Ext, RelPath: f.Rel, SHA256: sum}
		if err := e.Catalog.PutCompanion(ctx, cp); err != nil {
			return nil, err
		}
	}
	return src, nil
}

// sourceLocation names the source by volume UUID when one is available,
// so the same card is the same location however it mounts.
func (e *Env) sourceLocation(ctx context.Context, root string) (catalog.Location, error) {
	l := catalog.Location{Kind: catalog.Source, Root: root}
	if e.Identify != nil {
		if info, err := e.Identify(root); err == nil {
			l.VolumeUUID, l.Label = info.UUID, info.Label
		}
	}
	switch {
	case l.VolumeUUID != "":
		l.Name = "source:" + l.VolumeUUID
	case l.Label != "":
		l.Name = "source:" + l.Label
	default:
		l.Name = "source:" + root
	}
	return e.Catalog.UpsertLocation(ctx, l)
}

// register makes sure f has an asset row and a source copy row, computing
// identity and name only when the source-file cache misses.
func (e *Env) register(ctx context.Context, loc catalog.Location, f scan.File) (catalog.Asset, bool, error) {
	if id, ok, err := e.Catalog.LookupSourceFile(ctx, loc.ID, f.Rel, f.Size, f.ModTime); err != nil {
		return catalog.Asset{}, false, err
	} else if ok {
		a, err := e.Catalog.Asset(ctx, id)
		if err == nil {
			err = e.Catalog.PutCopy(ctx, catalog.Copy{AssetID: a.ID, LocationID: loc.ID, RelPath: f.Rel, State: catalog.Complete})
		}
		return a, false, err
	}

	a := catalog.Asset{Kind: f.Kind, Size: f.Size, OrigName: f.Name()}
	var err error
	if f.Kind.UsesSparseID() {
		var id identity.ID
		id, _, err = identity.SparseFile(f.Abs)
		a.ID, a.Scheme = string(id), identity.Scheme
	} else {
		a.ID, err = identity.FullFile(f.Abs)
		a.Scheme, a.FullSHA256 = "sha256", a.ID
	}
	if err != nil {
		return catalog.Asset{}, false, err
	}
	isNew := true
	if have, err := e.Catalog.Asset(ctx, a.ID); err == nil {
		// Same content seen before under another path or on another card.
		a, isNew = have, false
	} else if !errors.Is(err, catalog.ErrNotFound) {
		return catalog.Asset{}, false, err
	} else {
		a.CaptureTime, _ = scan.CaptureTime(f, e.loc)
		if a.RelPath, err = e.place(ctx, a); err != nil {
			return catalog.Asset{}, false, err
		}
	}
	if err := e.Catalog.PutCopy(ctx, catalog.Copy{AssetID: a.ID, LocationID: loc.ID, RelPath: f.Rel, State: catalog.Complete}); err != nil {
		return catalog.Asset{}, false, err
	}
	if err := e.Catalog.PutSourceFile(ctx, catalog.SourceFile{LocationID: loc.ID, Path: f.Rel, Size: f.Size, ModTime: f.ModTime, AssetID: a.ID}); err != nil {
		return catalog.Asset{}, false, err
	}
	return a, isNew, nil
}

// place chooses the permanent relpath for a new asset and records it,
// adding _1, _2… when a different asset already owns the name.
func (e *Env) place(ctx context.Context, a catalog.Asset) (string, error) {
	na := naming.Asset{Kind: a.Kind, OrigName: a.OrigName, ID: identity.ID(a.ID), CaptureTime: a.CaptureTime}
	rel, err := naming.DateScheme{}.Path(na)
	if err != nil {
		return "", err
	}
	for n := 0; n < 100; n++ {
		try := rel
		if n > 0 {
			try = naming.WithSuffix(rel, n)
		}
		a.RelPath = try
		err := e.Catalog.PutAsset(ctx, a)
		if err == nil {
			return try, nil
		}
		if !errors.Is(err, catalog.ErrPathTaken) {
			return "", err
		}
	}
	return "", fmt.Errorf("ingest: could not find a free name for %s near %s", a.OrigName, rel)
}

// sourceRoots returns the root of every catalogued source that is mounted
// right now. Without Identify a source counts as mounted when its recorded
// root exists; with it, the volume UUID must also match, because cards
// love to mount at the same path as the last card.
func (e *Env) sourceRoots(ctx context.Context) (map[int64]string, error) {
	locs, err := e.Catalog.AllLocations(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for _, l := range locs {
		if l.Kind != catalog.Source || l.Root == "" {
			continue
		}
		if st, err := os.Stat(l.Root); err != nil || !st.IsDir() {
			continue
		}
		if e.Identify != nil && l.VolumeUUID != "" {
			if info, err := e.Identify(l.Root); err != nil || info.UUID != l.VolumeUUID {
				continue
			}
		}
		out[l.ID] = l.Root
	}
	return out, nil
}

func videoOnly(as []catalog.Asset) []catalog.Asset {
	var out []catalog.Asset
	for _, a := range as {
		if a.Kind == media.Video {
			out = append(out, a)
		}
	}
	return out
}
