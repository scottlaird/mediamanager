package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"time"

	"github.com/scottlaird/mediamanager/catalog"
	"github.com/scottlaird/mediamanager/copyfile"
	"github.com/scottlaird/mediamanager/identity"
	"github.com/scottlaird/mediamanager/linktree"
	"github.com/scottlaird/mediamanager/naming"
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
		if bad, err := linktree.Audit(root); err != nil {
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
				proxies, err := e.Catalog.Proxies(ctx, a.ID)
				if err != nil {
					return rep, err
				}
				for ext, target := range bestProxies(proxies, locByID, rootOf) {
					want[naming.ProxyPath(a.RelPath, ext)] = target
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

// bestProxies picks, per extension, the proxy on the best mounted location
// in the same spool, NAS, source order the catalog uses for copies.
func bestProxies(proxies []catalog.Proxy, locByID map[int64]catalog.Location, rootOf func(catalog.Location) (string, bool)) map[string]string {
	rank := func(k catalog.LocationKind) int {
		switch k {
		case catalog.Spool:
			return 0
		case catalog.NAS:
			return 1
		}
		return 2
	}
	sort.SliceStable(proxies, func(i, j int) bool {
		li, lj := locByID[proxies[i].LocationID], locByID[proxies[j].LocationID]
		if rank(li.Kind) != rank(lj.Kind) {
			return rank(li.Kind) < rank(lj.Kind)
		}
		return li.Priority < lj.Priority
	})
	out := map[string]string{}
	for _, p := range proxies {
		if _, done := out[p.Ext]; done {
			continue
		}
		if root, ok := rootOf(locByID[p.LocationID]); ok {
			out[p.Ext] = abs(root, p.RelPath)
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
			results = append(results, CopyResult{AssetID: assetID, Location: p.cat.Name, Skipped: true})
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
	e.logf("copy %s -> %s", src, dst)
	res, err := copyfile.Copy(ctx, src, dst, copyfile.Options{
		ExpectID: expect,
		Progress: func(done, total int64) { e.logf("  %s: %d%%", a.RelPath, done*100/max(total, 1)) },
	})
	if err != nil {
		return CopyResult{}, fmt.Errorf("%s -> %s: %w", a.RelPath, dest.cat.Name, err)
	}
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
	n := e.copyProxies(ctx, a, rootOf, dest)
	if _, err := e.Reconcile(ctx, ps); err != nil {
		return CopyResult{}, err
	}
	return CopyResult{AssetID: a.ID, Location: dest.cat.Name, Bytes: res.Size, Resumed: res.Resumed, Proxies: n}, nil
}

// copyProxies brings the asset's proxies along to dest. Failures are
// logged, not returned: a proxy can always be regenerated.
func (e *Env) copyProxies(ctx context.Context, a catalog.Asset, rootOf func(catalog.Location) (string, bool), dest place) int {
	proxies, err := e.Catalog.Proxies(ctx, a.ID)
	if err != nil || len(proxies) == 0 {
		return 0
	}
	locs, _ := e.Catalog.AllLocations(ctx)
	locByID := map[int64]catalog.Location{}
	for _, l := range locs {
		locByID[l.ID] = l
	}
	t, _ := e.Config.Tree(a.Kind)
	n := 0
	for ext, src := range bestProxies(proxies, locByID, rootOf) {
		rel := path.Join(t.Subdir, naming.ProxyPath(a.RelPath, ext))
		if _, err := copyfile.Copy(ctx, src, abs(dest.root, rel), copyfile.Options{}); err != nil {
			e.logf("warning: proxy %s: %v", src, err)
			continue
		}
		if err := e.Catalog.PutProxy(ctx, catalog.Proxy{AssetID: a.ID, LocationID: dest.cat.ID, RelPath: rel, Ext: ext}); err != nil {
			e.logf("warning: recording proxy %s: %v", rel, err)
			continue
		}
		n++
	}
	return n
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
		e.dropSpoolProxies(ctx, a, *spool)
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

// dropSpoolProxies removes the asset's proxies from a spool being flushed.
// Proxies are regenerable, so no NAS copy is required first.
func (e *Env) dropSpoolProxies(ctx context.Context, a catalog.Asset, spool place) {
	proxies, err := e.Catalog.Proxies(ctx, a.ID)
	if err != nil {
		return
	}
	for _, p := range proxies {
		if p.LocationID != spool.cat.ID {
			continue
		}
		if err := os.Remove(abs(spool.root, p.RelPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			e.logf("warning: removing proxy %s: %v", p.RelPath, err)
			continue
		}
		_ = e.Catalog.DeleteProxy(ctx, a.ID, spool.cat.ID, p.Ext)
	}
}
