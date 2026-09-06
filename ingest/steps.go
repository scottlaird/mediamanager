package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/scottlaird/mediamanager/catalog"
	"github.com/scottlaird/mediamanager/copyfile"
	"github.com/scottlaird/mediamanager/identity"
	"github.com/scottlaird/mediamanager/linktree"
	"github.com/scottlaird/mediamanager/media"
	"github.com/scottlaird/mediamanager/naming"
	"github.com/scottlaird/mediamanager/scan"
)

// ErrSpoolFull means no mounted spool has room for the asset.
var ErrSpoolFull = errors.New("ingest: no spool has room")

// spoolMargin is free space to leave behind after a spool copy.
const spoolMargin = 256 << 20

// ReconcileReport is what Reconcile changed, per link tree root.
type ReconcileReport struct {
	Trees map[string]linktree.Report
	// Unavailable are assets with no usable copy right now (only on an
	// unplugged card, say). Their existing links are left as they are.
	Unavailable []string
}

// Reconcile audits every link tree and then points each asset's link at
// its best complete copy. Kinds configured with the same link root share
// one tree and are reconciled together, so neither sees the other's links
// as strays. It is safe to call at any time and from concurrent steps;
// calls are serialised.
func (e *Env) Reconcile(ctx context.Context, ps []place) (ReconcileReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rep := ReconcileReport{Trees: map[string]linktree.Report{}}
	srcRoots, err := e.sourceRoots(ctx)
	if err != nil {
		return rep, err
	}
	rootOf := e.rootOf(ps, srcRoots)
	locs, err := e.Catalog.AllLocations(ctx)
	if err != nil {
		return rep, err
	}
	locByID := map[int64]catalog.Location{}
	for _, l := range locs {
		locByID[l.ID] = l
	}

	for _, root := range e.linkRoots() {
		bad, err := linktree.Audit(root)
		if errors.Is(err, linktree.ErrRealFiles) {
			if err = e.sweepSidecars(ctx, root, bad, rootOf); err != nil {
				return rep, err
			}
			bad, err = linktree.Audit(root)
		}
		if err != nil {
			return rep, fmt.Errorf("%w in %s: %v", err, root, bad)
		}
		want := map[string]string{}
		for _, kind := range e.kindsLinkedAt(root) {
			assets, err := e.Catalog.AssetsByKind(ctx, kind)
			if err != nil {
				return rep, err
			}
			for _, a := range assets {
				copies, err := e.Catalog.Copies(ctx, a.ID)
				if err != nil {
					return rep, err
				}
				target, ok := linktree.Choose(copies, rootOf)
				if !ok {
					rep.Unavailable = append(rep.Unavailable, a.ID)
					continue
				}
				want[a.RelPath] = target
				comps, err := e.Catalog.Companions(ctx, a.ID)
				if err != nil {
					return rep, err
				}
				for key, target := range bestCompanions(comps, locByID, rootOf) {
					want[companionPath(a.RelPath, key.role, key.ext)] = target
				}
			}
		}
		lr, err := linktree.Reconcile(root, want, linktree.Options{})
		if err != nil {
			return rep, err
		}
		rep.Trees[root] = lr
	}
	return rep, nil
}

// companionKey identifies one companion of an asset across locations.
type companionKey struct {
	role catalog.Role
	ext  string
}

// companionPath is where a companion of the original at rel belongs.
func companionPath(rel string, role catalog.Role, ext string) string {
	switch role {
	case catalog.RoleSidecar:
		return naming.SidecarPath(rel, ext)
	case catalog.RoleProxySidecar:
		return naming.ProxySidecarPath(rel, ext)
	default:
		return naming.ProxyPath(rel, ext)
	}
}

// bestCompanions picks, per role and extension, the companion on the best
// mounted location in the same spool, NAS, source order the catalog uses
// for copies.
func bestCompanions(comps []catalog.Companion, locByID map[int64]catalog.Location, rootOf func(catalog.Location) (string, bool)) map[companionKey]string {
	rank := func(k catalog.LocationKind) int {
		switch k {
		case catalog.Spool:
			return 0
		case catalog.NAS:
			return 1
		}
		return 2
	}
	sort.SliceStable(comps, func(i, j int) bool {
		li, lj := locByID[comps[i].LocationID], locByID[comps[j].LocationID]
		if rank(li.Kind) != rank(lj.Kind) {
			return rank(li.Kind) < rank(lj.Kind)
		}
		return li.Priority < lj.Priority
	})
	out := map[companionKey]string{}
	for _, c := range comps {
		k := companionKey{c.Role, c.Ext}
		if _, done := out[k]; done {
			continue
		}
		if root, ok := rootOf(locByID[c.LocationID]); ok {
			out[k] = abs(root, c.RelPath)
		}
	}
	return out
}

// CopyResult is what Spool and Archive report for one asset.
type CopyResult struct {
	AssetID string
	// Location is where the copy landed.
	Location string
	// Skipped is set when a complete copy already existed there.
	Skipped bool
	Bytes   int64
	Resumed int64
	Proxies int
}

// Spool copies an asset onto the first mounted spool with room for it,
// from its best available copy. A complete spool copy already present on
// a mounted spool makes this a no-op.
func (e *Env) Spool(ctx context.Context, ps []place, assetID string) (CopyResult, error) {
	e.init()
	defer e.lockAsset(assetID)()
	a, copies, rootOf, err := e.lookup(ctx, ps, assetID)
	if err != nil {
		return CopyResult{}, err
	}
	for _, cp := range copies {
		if cp.State == catalog.Complete && cp.Location.Kind == catalog.Spool {
			if _, ok := rootOf(cp.Location); ok {
				return CopyResult{AssetID: assetID, Location: cp.Location.Name, Skipped: true}, nil
			}
		}
	}
	var dest *place
	for i := range ps {
		p := &ps[i]
		if !p.mounted || p.cat.Kind != catalog.Spool {
			continue
		}
		free, err := freeSpace(p.root)
		if err != nil {
			return CopyResult{}, err
		}
		if free >= a.Size+spoolMargin {
			dest = p
			break
		}
	}
	if dest == nil {
		return CopyResult{}, fmt.Errorf("%w: %s needs %d bytes", ErrSpoolFull, a.RelPath, a.Size)
	}
	return e.copyTo(ctx, ps, a, copies, rootOf, *dest)
}

// Archive copies an asset onto every mounted NAS that lacks it, holding
// the NAS-wide concurrency slot for the duration of each copy.
func (e *Env) Archive(ctx context.Context, ps []place, assetID string) ([]CopyResult, error) {
	e.init()
	defer e.lockAsset(assetID)()
	a, copies, rootOf, err := e.lookup(ctx, ps, assetID)
	if err != nil {
		return nil, err
	}
	var results []CopyResult
	for _, p := range ps {
		if !p.mounted || p.cat.Kind != catalog.NAS {
			continue
		}
		if hasComplete(copies, p.cat.ID) {
			// The original never changes, but sidecars do: keep the NAS copy current.
			n := e.syncCompanions(ctx, a, rootOf, p, true)
			results = append(results, CopyResult{AssetID: assetID, Location: p.cat.Name, Skipped: true, Proxies: n})
			continue
		}
		select {
		case e.nas <- struct{}{}:
		case <-ctx.Done():
			return results, ctx.Err()
		}
		r, err := e.copyTo(ctx, ps, a, copies, rootOf, p)
		<-e.nas
		if err != nil {
			return results, err
		}
		results = append(results, r)
	}
	return results, nil
}

func hasComplete(copies []catalog.Copy, locID int64) bool {
	for _, cp := range copies {
		if cp.LocationID == locID && cp.State == catalog.Complete {
			return true
		}
	}
	return false
}

func (e *Env) lookup(ctx context.Context, ps []place, assetID string) (catalog.Asset, []catalog.Copy, func(catalog.Location) (string, bool), error) {
	a, err := e.Catalog.Asset(ctx, assetID)
	if err != nil {
		return a, nil, nil, err
	}
	copies, err := e.Catalog.Copies(ctx, assetID)
	if err != nil {
		return a, nil, nil, err
	}
	srcRoots, err := e.sourceRoots(ctx)
	if err != nil {
		return a, nil, nil, err
	}
	return a, copies, e.rootOf(ps, srcRoots), nil
}

// copyTo performs one tier transition for an asset and its proxies,
// recording partial before and complete after, and reconciling the link
// tree when done.
func (e *Env) copyTo(ctx context.Context, ps []place, a catalog.Asset, copies []catalog.Copy, rootOf func(catalog.Location) (string, bool), dest place) (CopyResult, error) {
	src, ok := linktree.Choose(copies, rootOf)
	if !ok {
		return CopyResult{}, fmt.Errorf("ingest: no readable copy of %s is mounted", a.RelPath)
	}
	rel, err := e.treeRel(a)
	if err != nil {
		return CopyResult{}, err
	}
	dst := abs(dest.root, rel)
	if err := e.Catalog.PutCopy(ctx, catalog.Copy{AssetID: a.ID, LocationID: dest.cat.ID, RelPath: rel, State: catalog.Partial}); err != nil {
		return CopyResult{}, err
	}
	var expect identity.ID
	if a.Kind.UsesSparseID() {
		expect = identity.ID(a.ID)
	}
	// Copies are numbered so interleaved progress from concurrent copies
	// reads as separate streams.
	label := fmt.Sprintf("#%d %s", e.copySeq.Add(1), path.Base(a.RelPath))
	e.logf("%s: copy %s -> %s (%s)", label, src, dst, fmtBytes(a.Size))
	var prog *progress
	res, err := copyfile.Copy(ctx, src, dst, copyfile.Options{
		ExpectID: expect,
		Progress: func(done, total int64) {
			if prog == nil {
				// The first report arrives after one buffer past any resumed
				// prefix; rates are measured from there.
				prog = newProgress(e.Logf, label, done)
				return
			}
			prog.report(done, total)
		},
	})
	if err != nil {
		return CopyResult{}, fmt.Errorf("%s -> %s: %w", a.RelPath, dest.cat.Name, err)
	}
	if res.Resumed > 0 {
		e.logf("%s: resumed, %s was already at %s", label, fmtBytes(res.Resumed), dest.cat.Name)
	}
	e.logf("%s: done -> %s", label, dest.cat.Name)
	full := res.FullSHA256
	if full == "" {
		full = a.FullSHA256
	}
	cp := catalog.Copy{AssetID: a.ID, LocationID: dest.cat.ID, RelPath: rel, State: catalog.Complete, FullSHA256: full, VerifiedAt: time.Now()}
	if err := e.Catalog.PutCopy(ctx, cp); err != nil {
		return CopyResult{}, err
	}
	if a.FullSHA256 == "" && res.FullSHA256 != "" {
		if err := e.Catalog.SetFullSHA256(ctx, a.ID, res.FullSHA256); err != nil {
			return CopyResult{}, err
		}
	}
	n := e.syncCompanions(ctx, a, rootOf, dest, false)
	if _, err := e.Reconcile(ctx, ps); err != nil {
		return CopyResult{}, err
	}
	return CopyResult{AssetID: a.ID, Location: dest.cat.Name, Bytes: res.Size, Resumed: res.Resumed, Proxies: n}, nil
}

// syncCompanions brings the asset's companions to dest and returns how
// many it wrote. Proxies are copied once and never replaced; sidecars are
// rewritten whenever the best copy's content differs, because editors
// change them after import. With sidecarsOnly, proxies are left alone.
// Failures are logged, not returned: nothing here blocks the original.
func (e *Env) syncCompanions(ctx context.Context, a catalog.Asset, rootOf func(catalog.Location) (string, bool), dest place, sidecarsOnly bool) int {
	comps, err := e.Catalog.Companions(ctx, a.ID)
	if err != nil || len(comps) == 0 {
		return 0
	}
	locs, _ := e.Catalog.AllLocations(ctx)
	locByID := map[int64]catalog.Location{}
	for _, l := range locs {
		locByID[l.ID] = l
	}
	// Prefer a copy from anywhere but dest itself as the source of truth.
	srcRoot := func(l catalog.Location) (string, bool) {
		if l.ID == dest.cat.ID {
			return "", false
		}
		return rootOf(l)
	}
	t, _ := e.Config.Tree(a.Kind)
	n := 0
	for key, src := range bestCompanions(comps, locByID, srcRoot) {
		if sidecarsOnly && key.role.Regenerable() {
			continue
		}
		rel := path.Join(t.Subdir, companionPath(a.RelPath, key.role, key.ext))
		dst := abs(dest.root, rel)
		var sum string
		if key.role.Regenerable() {
			if _, err := copyfile.Copy(ctx, src, dst, copyfile.Options{}); err != nil {
				e.logf("warning: proxy %s: %v", src, err)
				continue
			}
			sum, _ = identity.FullFile(dst)
		} else {
			changed, err := replaceIfDifferent(src, dst)
			if err != nil {
				e.logf("warning: sidecar %s: %v", src, err)
				continue
			}
			if !changed {
				continue
			}
			sum, _ = identity.FullFile(dst)
		}
		if err := e.Catalog.PutCompanion(ctx, catalog.Companion{AssetID: a.ID, LocationID: dest.cat.ID, Role: key.role, Ext: key.ext, RelPath: rel, SHA256: sum}); err != nil {
			e.logf("warning: recording companion %s: %v", rel, err)
			continue
		}
		n++
	}
	return n
}

// replaceIfDifferent makes dst a copy of src unless it already is one,
// writing through a temporary name so dst is never half-written. This is
// the one place the tool overwrites a file on any tier, and it is only ever
// used for sidecars.
func replaceIfDifferent(src, dst string) (bool, error) {
	want, err := identity.FullFile(src)
	if err != nil {
		return false, err
	}
	if have, err := identity.FullFile(dst); err == nil && have == want {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return false, err
	}
	tmp := dst + copyfile.PartialSuffix
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return false, err
	}
	if st, err := os.Stat(src); err == nil {
		_ = os.Chtimes(tmp, st.ModTime(), st.ModTime())
	}
	return true, os.Rename(tmp, dst)
}

// FlushOptions select what to remove from a spool.
type FlushOptions struct {
	// FreeBytes stops the flush once this much has been freed. Zero means
	// flush every eligible asset.
	FreeBytes int64
	// OlderThan, when set, only flushes assets captured before it.
	OlderThan time.Time
	// DryRun reports what would be flushed without deleting.
	DryRun bool
}

// FlushReport is what Flush did.
type FlushReport struct {
	Flushed []string
	Freed   int64
	// Refused lists assets whose NAS or spool copy failed verification and
	// were therefore kept, with the reason.
	Refused map[string]string
}

// Flush deletes spool copies of assets that are verified to exist on the
// NAS, oldest capture first, and repoints their links. Rule R4: both the
// NAS copy and the spool copy are re-verified by size and sparse identity
// immediately before the delete, and recorded full hashes must agree.
func (e *Env) Flush(ctx context.Context, ps []place, spoolName string, opts FlushOptions) (FlushReport, error) {
	e.init()
	rep := FlushReport{Refused: map[string]string{}}
	var spool *place
	for i := range ps {
		if ps[i].cat.Name == spoolName && ps[i].cat.Kind == catalog.Spool {
			spool = &ps[i]
		}
	}
	if spool == nil {
		return rep, fmt.Errorf("ingest: no spool named %q", spoolName)
	}
	if !spool.mounted {
		return rep, fmt.Errorf("ingest: spool %q is not mounted", spoolName)
	}
	srcRoots, err := e.sourceRoots(ctx)
	if err != nil {
		return rep, err
	}
	rootOf := e.rootOf(ps, srcRoots)

	candidates, err := e.Catalog.Flushable(ctx, spool.cat.ID)
	if err != nil {
		return rep, err
	}
	for _, a := range candidates {
		if opts.FreeBytes > 0 && rep.Freed >= opts.FreeBytes {
			break
		}
		if !opts.OlderThan.IsZero() && !a.CaptureTime.Before(opts.OlderThan) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		copies, err := e.Catalog.Copies(ctx, a.ID)
		if err != nil {
			return rep, err
		}
		spoolCopy, nasCopy, reason := pickFlushPair(copies, spool.cat.ID, rootOf)
		if reason == "" {
			reason = verifyPair(a, abs(spool.root, spoolCopy.RelPath), abs(mustRoot(rootOf, nasCopy.Location), nasCopy.RelPath), spoolCopy, nasCopy)
		}
		if reason != "" {
			rep.Refused[a.ID] = reason
			e.logf("keeping %s: %s", a.RelPath, reason)
			continue
		}
		if opts.DryRun {
			rep.Flushed = append(rep.Flushed, a.ID)
			rep.Freed += a.Size
			continue
		}
		if err := os.Remove(abs(spool.root, spoolCopy.RelPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return rep, err
		}
		if err := e.Catalog.DeleteCopy(ctx, a.ID, spool.cat.ID); err != nil {
			return rep, err
		}
		e.dropSpoolCompanions(ctx, a, *spool, rootOf)
		rep.Flushed = append(rep.Flushed, a.ID)
		rep.Freed += a.Size
	}
	if !opts.DryRun && len(rep.Flushed) > 0 {
		if _, err := e.Reconcile(ctx, ps); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func pickFlushPair(copies []catalog.Copy, spoolID int64, rootOf func(catalog.Location) (string, bool)) (spoolCopy, nasCopy catalog.Copy, reason string) {
	var haveSpool, haveNAS bool
	for _, cp := range copies {
		if cp.State != catalog.Complete {
			continue
		}
		switch {
		case cp.LocationID == spoolID:
			spoolCopy, haveSpool = cp, true
		case cp.Location.Kind == catalog.NAS && !haveNAS:
			if _, ok := rootOf(cp.Location); ok {
				nasCopy, haveNAS = cp, true
			}
		}
	}
	switch {
	case !haveSpool:
		return spoolCopy, nasCopy, "no complete spool copy recorded"
	case !haveNAS:
		return spoolCopy, nasCopy, "no mounted NAS copy"
	}
	return spoolCopy, nasCopy, ""
}

// verifyPair re-checks both files and returns a reason to refuse, or "".
func verifyPair(a catalog.Asset, spoolPath, nasPath string, spoolCopy, nasCopy catalog.Copy) string {
	if spoolCopy.FullSHA256 != "" && nasCopy.FullSHA256 != "" && spoolCopy.FullSHA256 != nasCopy.FullSHA256 {
		return "recorded full hashes differ"
	}
	nasID, nasSize, err := identity.SparseFile(nasPath)
	if err != nil {
		return "NAS copy unreadable: " + err.Error()
	}
	spoolID, spoolSize, err := identity.SparseFile(spoolPath)
	if err != nil {
		return "spool copy unreadable: " + err.Error()
	}
	switch {
	case nasSize != a.Size:
		return fmt.Sprintf("NAS copy is %d bytes, want %d", nasSize, a.Size)
	case spoolSize != a.Size:
		return fmt.Sprintf("spool copy is %d bytes, want %d", spoolSize, a.Size)
	case nasID != spoolID:
		return "NAS and spool copies differ"
	case a.Kind.UsesSparseID() && string(nasID) != a.ID:
		return "NAS copy does not match asset identity"
	}
	return ""
}

func mustRoot(rootOf func(catalog.Location) (string, bool), l catalog.Location) string {
	r, _ := rootOf(l)
	return r
}

// dropSpoolCompanions removes the asset's companions from a spool being
// flushed. Proxies just go. A sidecar is first brought up to date on the
// NAS (it may have been edited since it was archived) and is kept if that
// fails, so a sidecar is never the last copy that gets deleted.
func (e *Env) dropSpoolCompanions(ctx context.Context, a catalog.Asset, spool place, rootOf func(catalog.Location) (string, bool)) {
	comps, err := e.Catalog.Companions(ctx, a.ID)
	if err != nil {
		return
	}
	locs, _ := e.Catalog.AllLocations(ctx)
	locByID := map[int64]catalog.Location{}
	for _, l := range locs {
		locByID[l.ID] = l
	}
	t, _ := e.Config.Tree(a.Kind)
	for _, c := range comps {
		if c.LocationID != spool.cat.ID {
			continue
		}
		src := abs(spool.root, c.RelPath)
		if !c.Role.Regenerable() {
			if !e.sidecarSafeOnNAS(ctx, a, c, src, t.Subdir, locByID, rootOf) {
				e.logf("keeping sidecar %s: no verified NAS copy", c.RelPath)
				continue
			}
		}
		if err := os.Remove(src); err != nil && !errors.Is(err, os.ErrNotExist) {
			e.logf("warning: removing %s: %v", c.RelPath, err)
			continue
		}
		_ = e.Catalog.DeleteCompanion(ctx, a.ID, spool.cat.ID, c.Role, c.Ext)
	}
}

// sidecarSafeOnNAS makes sure some mounted NAS holds a byte-identical copy
// of the sidecar at src, writing it if needed, and reports success.
func (e *Env) sidecarSafeOnNAS(ctx context.Context, a catalog.Asset, c catalog.Companion, src, subdir string, locByID map[int64]catalog.Location, rootOf func(catalog.Location) (string, bool)) bool {
	for _, l := range locByID {
		if l.Kind != catalog.NAS {
			continue
		}
		root, ok := rootOf(l)
		if !ok {
			continue
		}
		rel := path.Join(subdir, companionPath(a.RelPath, c.Role, c.Ext))
		dst := abs(root, rel)
		if _, err := replaceIfDifferent(src, dst); err != nil {
			e.logf("warning: syncing sidecar to %s: %v", l.Name, err)
			continue
		}
		sum, _ := identity.FullFile(dst)
		if err := e.Catalog.PutCompanion(ctx, catalog.Companion{AssetID: a.ID, LocationID: l.ID, Role: c.Role, Ext: c.Ext, RelPath: rel, SHA256: sum}); err != nil {
			continue
		}
		return true
	}
	return false
}

// sweepSidecars handles the one kind of real file that legitimately turns
// up in a link tree: a sidecar an editor just created beside the link it
// opened (Resolve's .sidecar, Lightroom's .xmp). Each is moved into storage
// beside the copy the link points at, recorded as a companion, and then
// linked like any other by the reconcile that follows. Any other real file
// is left for the audit to reject.
func (e *Env) sweepSidecars(ctx context.Context, root string, offenders []string, rootOf func(catalog.Location) (string, bool)) error {
	for _, rel := range offenders {
		ext := strings.ToLower(strings.TrimPrefix(path.Ext(rel), "."))
		if !scan.IsSidecarExt(ext) {
			continue
		}
		a, role, ok, err := e.sidecarOwner(ctx, root, rel)
		if err != nil {
			return err
		}
		if !ok {
			continue // no sibling link; the audit will report it
		}
		copies, err := e.Catalog.Copies(ctx, a.ID)
		if err != nil {
			return err
		}
		var dest *catalog.Copy
		for i := range copies {
			if copies[i].State == catalog.Complete {
				if _, mounted := rootOf(copies[i].Location); mounted && copies[i].Location.Kind != catalog.Source {
					dest = &copies[i]
					break
				}
			}
		}
		if dest == nil {
			e.logf("warning: %s: no spool or NAS copy of %s is mounted; sidecar left in place", rel, a.RelPath)
			continue
		}
		t, _ := e.Config.Tree(a.Kind)
		destRoot, _ := rootOf(dest.Location)
		destRel := path.Join(t.Subdir, companionPath(a.RelPath, role, ext))
		src := abs(root, rel)
		if _, err := replaceIfDifferent(src, abs(destRoot, destRel)); err != nil {
			return fmt.Errorf("sweeping %s: %w", rel, err)
		}
		if err := os.Remove(src); err != nil {
			return err
		}
		sum, _ := identity.FullFile(abs(destRoot, destRel))
		if err := e.Catalog.PutCompanion(ctx, catalog.Companion{AssetID: a.ID, LocationID: dest.LocationID, Role: role, Ext: ext, RelPath: destRel, SHA256: sum}); err != nil {
			return err
		}
		e.logf("swept %s into %s", rel, dest.Location.Name)
	}
	return nil
}

// sidecarOwner finds the asset a stray sidecar belongs to: the link beside
// it (same directory, same base) resolves to an asset path, or for a file
// under Proxy/, the proxy link beside it does.
func (e *Env) sidecarOwner(ctx context.Context, root, rel string) (catalog.Asset, catalog.Role, bool, error) {
	dir, name := path.Split(rel)
	base := strings.TrimSuffix(name, path.Ext(name))
	role := catalog.RoleSidecar
	assetDir := dir
	if strings.EqualFold(path.Base(path.Clean(dir)), "proxy") {
		role = catalog.RoleProxySidecar
		assetDir = path.Dir(path.Clean(dir)) + "/"
		if assetDir == "./" {
			assetDir = ""
		}
	}
	entries, err := os.ReadDir(abs(root, assetDir))
	if err != nil {
		return catalog.Asset{}, "", false, err
	}
	var candidates []catalog.Asset
	for _, ent := range entries {
		if ent.Type()&os.ModeSymlink == 0 {
			continue
		}
		n := ent.Name()
		if strings.TrimSuffix(n, path.Ext(n)) != base {
			continue
		}
		for _, kind := range e.kindsLinkedAt(root) {
			if a, err := e.Catalog.AssetByPath(ctx, kind, path.Join(assetDir, n)); err == nil {
				candidates = append(candidates, a)
			}
		}
	}
	if len(candidates) == 0 {
		return catalog.Asset{}, "", false, nil
	}
	return preferredOwner(candidates, strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))), role, true, nil
}

// preferredOwner picks which of several same-base originals a sidecar
// belongs to: .sidecar goes with video, .xmp with a raw still before a
// JPEG, and otherwise the first candidate.
func preferredOwner(cands []catalog.Asset, ext string) catalog.Asset {
	score := func(a catalog.Asset) int {
		aext := strings.ToLower(strings.TrimPrefix(path.Ext(a.RelPath), "."))
		switch {
		case ext == "sidecar" && a.Kind == media.Video:
			return 3
		case ext == "xmp" && a.Kind == media.Still && aext != "jpg" && aext != "jpeg":
			return 3
		case ext == "xmp" && a.Kind == media.Still:
			return 2
		case a.Kind == media.Video:
			return 1
		}
		return 0
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if score(c) > score(best) {
			best = c
		}
	}
	return best
}
