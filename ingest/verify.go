package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/scottlaird/mediamanager/catalog"
	"github.com/scottlaird/mediamanager/copyfile"
	"github.com/scottlaird/mediamanager/identity"
)

// VerifyOptions tune Verify.
type VerifyOptions struct {
	// Full re-hashes every byte and compares to the recorded full hash
	// where one exists. Without it, video and audio copies are checked by
	// size and sparse identity, stills by size only.
	Full bool
	// Location restricts the check to copies on one location.
	Location string
	// AllowUpdates lets a changed still be adopted as the new content of
	// its asset (see Verify). Never the default.
	AllowUpdates bool
}

// VerifyItem is one copy that did not verify, or was updated.
type VerifyItem struct {
	Path     string
	Location string
	// Result is one of: missing, size, identity, hash, updated, replaced,
	// unresolved.
	Result string
	Detail string
}

// VerifyReport is what Verify found.
type VerifyReport struct {
	Copies   int
	OK       int
	Missing  int
	Mismatch int
	Updated  int
	Items    []VerifyItem
}

// Verify re-checks the mounted spool and NAS copies of the given assets
// against the catalog. A copy that no longer matches is marked Mismatch:
// it is then never linked to, never counts as a safe copy for flush, and
// is never deleted by the tool. Nothing on disk changes unless
// AllowUpdates is set, in which case a still with exactly one changed copy
// and every other copy intact is treated as edited in place: the catalog
// takes the new size and hash, and the intact copies are replaced with
// the new content. Video and audio are never updated, since their
// identity is in the filename; a change there is reported and left.
func (e *Env) Verify(ctx context.Context, refs []AssetRef, opts VerifyOptions) (*VerifyReport, error) {
	e.init()
	ps, err := e.places(ctx)
	if err != nil {
		return nil, err
	}
	rootOf := e.rootOf(ps, nil)
	rep := &VerifyReport{}
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		a, err := e.Catalog.Asset(ctx, ref.ID)
		if err != nil {
			return rep, err
		}
		copies, err := e.Catalog.Copies(ctx, a.ID)
		if err != nil {
			return rep, err
		}
		var changed []catalog.Copy
		var intact []catalog.Copy
		for _, cp := range copies {
			if cp.Location.Kind == catalog.Source || cp.State == catalog.Partial {
				continue
			}
			if opts.Location != "" && cp.Location.Name != opts.Location {
				continue
			}
			root, mounted := rootOf(cp.Location)
			if !mounted {
				continue
			}
			rep.Copies++
			it, ok := e.checkCopy(a, cp, abs(root, cp.RelPath), opts.Full)
			if ok {
				rep.OK++
				intact = append(intact, cp)
				if cp.State != catalog.Complete {
					// A copy previously marked mismatched that now matches
					// (restored by hand) is complete again.
					_ = e.Catalog.SetCopyState(ctx, a.ID, cp.LocationID, catalog.Complete)
				} else {
					_ = e.Catalog.SetCopyState(ctx, a.ID, cp.LocationID, catalog.Complete) // stamps verified_at
				}
				continue
			}
			if it.Result == "missing" {
				rep.Missing++
			} else {
				rep.Mismatch++
			}
			rep.Items = append(rep.Items, it)
			if err := e.Catalog.SetCopyState(ctx, a.ID, cp.LocationID, catalog.Mismatch); err != nil {
				return rep, err
			}
			if it.Result != "missing" {
				changed = append(changed, cp)
			}
		}
		if opts.AllowUpdates && len(changed) == 1 && len(intact) > 0 {
			if err := e.adoptEdit(ctx, rep, a, changed[0], intact, rootOf); err != nil {
				return rep, err
			}
		}
	}
	if len(rep.Items) > 0 {
		if _, err := e.Reconcile(ctx, ps); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// checkCopy compares one file with the catalog. ok is true when it matches.
func (e *Env) checkCopy(a catalog.Asset, cp catalog.Copy, path string, full bool) (VerifyItem, bool) {
	it := VerifyItem{Path: a.Kind.String() + "/" + a.RelPath, Location: cp.Location.Name}
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		it.Result, it.Detail = "missing", path
		return it, false
	}
	if err != nil {
		it.Result, it.Detail = "missing", err.Error()
		return it, false
	}
	if st.Size() != a.Size {
		it.Result, it.Detail = "size", fmt.Sprintf("%d bytes on disk, catalog says %d", st.Size(), a.Size)
		return it, false
	}
	if a.Kind.UsesSparseID() {
		id, _, err := identity.SparseFile(path)
		if err != nil {
			it.Result, it.Detail = "missing", err.Error()
			return it, false
		}
		if string(id) != a.ID {
			it.Result, it.Detail = "identity", fmt.Sprintf("sparse identity %s, catalog says %s", id, a.ID)
			return it, false
		}
	}
	if full {
		want := a.FullSHA256
		if want == "" && !a.Kind.UsesSparseID() {
			want = a.ID // stills: the ID is the original hash
		}
		if want != "" {
			sum, err := identity.FullFile(path)
			if err != nil {
				it.Result, it.Detail = "missing", err.Error()
				return it, false
			}
			if sum != want {
				it.Result, it.Detail = "hash", fmt.Sprintf("sha256 %s…, catalog says %s…", sum[:16], want[:16])
				return it, false
			}
		}
	}
	return it, true
}

// adoptEdit takes the changed copy as the asset's new content and brings
// the intact copies up to it. Stills only.
func (e *Env) adoptEdit(ctx context.Context, rep *VerifyReport, a catalog.Asset, changed catalog.Copy, intact []catalog.Copy, rootOf func(catalog.Location) (string, bool)) error {
	if a.Kind.UsesSparseID() {
		rep.Items = append(rep.Items, VerifyItem{Path: a.Kind.String() + "/" + a.RelPath, Location: changed.Location.Name,
			Result: "unresolved", Detail: "video and audio are never updated in place; restore from an intact copy by hand"})
		return nil
	}
	root, _ := rootOf(changed.Location)
	src := abs(root, changed.RelPath)
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	sum, err := identity.FullFile(src)
	if err != nil {
		return err
	}
	if err := e.Catalog.SetAssetContent(ctx, a.ID, st.Size(), sum); err != nil {
		return err
	}
	if err := e.Catalog.PutCopy(ctx, catalog.Copy{AssetID: a.ID, LocationID: changed.LocationID, RelPath: changed.RelPath, State: catalog.Complete, FullSHA256: sum}); err != nil {
		return err
	}
	rep.Updated++
	rep.Items = append(rep.Items, VerifyItem{Path: a.Kind.String() + "/" + a.RelPath, Location: changed.Location.Name,
		Result: "updated", Detail: fmt.Sprintf("catalog now %d bytes, sha256 %s…", st.Size(), sum[:16])})
	for _, cp := range intact {
		r, _ := rootOf(cp.Location)
		dst := abs(r, cp.RelPath)
		if err := replaceFile(ctx, src, dst); err != nil {
			rep.Items = append(rep.Items, VerifyItem{Path: a.Kind.String() + "/" + a.RelPath, Location: cp.Location.Name,
				Result: "unresolved", Detail: "could not replace with the edited content: " + err.Error()})
			_ = e.Catalog.SetCopyState(ctx, a.ID, cp.LocationID, catalog.Mismatch)
			continue
		}
		if err := e.Catalog.PutCopy(ctx, catalog.Copy{AssetID: a.ID, LocationID: cp.LocationID, RelPath: cp.RelPath, State: catalog.Complete, FullSHA256: sum}); err != nil {
			return err
		}
		rep.Items = append(rep.Items, VerifyItem{Path: a.Kind.String() + "/" + a.RelPath, Location: cp.Location.Name, Result: "replaced", Detail: "now holds the edited content"})
	}
	return nil
}

// replaceFile puts a verified copy of src at dst, over whatever is there,
// through a temporary name. It is the one place the tool overwrites an
// original, and only under --allow-updates.
func replaceFile(ctx context.Context, src, dst string) error {
	tmp := dst + ".replace"
	_ = os.Remove(tmp)
	if _, err := copyfile.Copy(ctx, src, tmp, copyfile.Options{}); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
