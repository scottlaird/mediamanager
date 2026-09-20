package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

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
	// Parallelism is how many copies are read at once; 0 means the
	// configured concurrency.verify.
	Parallelism int
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

// VerifyReport is what Verify found, and how fast.
type VerifyReport struct {
	Copies   int
	OK       int
	Missing  int
	Mismatch int
	Updated  int
	Items    []VerifyItem
	// BytesRead is what the checks read; Duration is wall time for the
	// reading phase.
	BytesRead   int64
	Duration    time.Duration
	Parallelism int
	// Locations breaks the reading down per location, in config order.
	Locations []LocationStats
}

// LocationStats is verify's reading on one location. Duration is the sum
// of per-copy read times there, so with several readers it exceeds wall
// time; MiBPerSecond is bytes over the wall time during which that
// location was being read, which is the number to compare with a
// network graph.
type LocationStats struct {
	Location     string
	Kind         string
	Copies       int
	BytesRead    int64
	Wall         time.Duration
	MiBPerSecond float64
}

// MiBPerSecond is the aggregate read rate of the checking phase.
func (r *VerifyReport) MiBPerSecond() float64 {
	if r.Duration <= 0 {
		return 0
	}
	return float64(r.BytesRead) / (1 << 20) / r.Duration.Seconds()
}

// verifyJob is one copy to check.
type verifyJob struct {
	asset catalog.Asset
	copy  catalog.Copy
	path  string
}

type verifyResult struct {
	job        verifyJob
	item       VerifyItem
	ok         bool
	read       int64
	start, end time.Time
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
//
// Copies are read Parallelism at a time; catalog updates happen after all
// of an asset's copies have been checked.
func (e *Env) Verify(ctx context.Context, refs []AssetRef, opts VerifyOptions) (*VerifyReport, error) {
	e.init()
	ps, err := e.places(ctx)
	if err != nil {
		return nil, err
	}
	rootOf := e.rootOf(ps, nil)
	par := opts.Parallelism
	if par <= 0 {
		par = e.Config.Concurrency.Verify
	}
	if par <= 0 {
		par = 1
	}
	rep := &VerifyReport{Parallelism: par}

	// Gather the work first so progress can say how much there is.
	var jobs []verifyJob
	assets := map[string]catalog.Asset{}
	var total int64
	for _, ref := range refs {
		a, err := e.Catalog.Asset(ctx, ref.ID)
		if err != nil {
			return nil, err
		}
		assets[a.ID] = a
		copies, err := e.Catalog.Copies(ctx, a.ID)
		if err != nil {
			return nil, err
		}
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
			jobs = append(jobs, verifyJob{asset: a, copy: cp, path: abs(root, cp.RelPath)})
			total += bytesToRead(a, opts.Full)
		}
	}
	rep.Copies = len(jobs)
	e.logf("verify: %d copies, %s to read, %d at a time", len(jobs), fmtBytes(total), par)

	// Check in parallel; results are grouped per asset afterwards.
	start := time.Now()
	in := make(chan verifyJob)
	out := make(chan verifyResult, par)
	var wg sync.WaitGroup
	for i := 0; i < par; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range in {
				t0 := time.Now()
				it, ok := e.checkCopy(j.asset, j.copy, j.path, opts.Full)
				out <- verifyResult{job: j, item: it, ok: ok, read: bytesToRead(j.asset, opts.Full), start: t0, end: time.Now()}
			}
		}()
	}
	go func() {
		defer close(in)
		for _, j := range jobs {
			select {
			case in <- j:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(out) }()

	byAsset := map[string][]verifyResult{}
	var done, read atomic.Int64
	prog := newProgress(e.Logf, "verify", 0)
	perLoc := map[int64]*locAcc{}
	for r := range out {
		byAsset[r.job.asset.ID] = append(byAsset[r.job.asset.ID], r)
		done.Add(1)
		read.Add(r.read)
		prog.report(read.Load(), total)
		acc := perLoc[r.job.copy.LocationID]
		if acc == nil {
			acc = &locAcc{loc: r.job.copy.Location}
			perLoc[r.job.copy.LocationID] = acc
		}
		acc.add(r)
	}
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	rep.Duration = time.Since(start)
	rep.BytesRead = read.Load()
	for _, p := range ps {
		if acc := perLoc[p.cat.ID]; acc != nil {
			rep.Locations = append(rep.Locations, acc.stats())
		}
	}

	// Apply findings per asset, in a stable order.
	for _, ref := range refs {
		results := byAsset[ref.ID]
		if len(results) == 0 {
			continue
		}
		a := assets[ref.ID]
		var changed, intact []catalog.Copy
		for _, r := range results {
			cp := r.job.copy
			if r.ok {
				rep.OK++
				intact = append(intact, cp)
				// Stamps verified_at; also restores a copy fixed by hand.
				if err := e.Catalog.SetCopyState(ctx, a.ID, cp.LocationID, catalog.Complete); err != nil {
					return rep, err
				}
				continue
			}
			if r.item.Result == "missing" {
				rep.Missing++
			} else {
				rep.Mismatch++
				changed = append(changed, cp)
			}
			rep.Items = append(rep.Items, r.item)
			if err := e.Catalog.SetCopyState(ctx, a.ID, cp.LocationID, catalog.Mismatch); err != nil {
				return rep, err
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
	e.logf("verify: %d copies in %s, %s read at %.0f MiB/s", rep.Copies, rep.Duration.Round(time.Second), fmtBytes(rep.BytesRead), rep.MiBPerSecond())
	for _, l := range rep.Locations {
		e.logf("  %s (%s): %d copies, %s at %.0f MiB/s", l.Location, l.Kind, l.Copies, fmtBytes(l.BytesRead), l.MiBPerSecond)
	}
	return rep, nil
}

// locAcc accumulates verify reads on one location. Wall time is the union
// of the per-copy read intervals, so overlapping readers are not double
// counted and idle stretches (while other locations were being read) do
// not dilute the rate.
type locAcc struct {
	loc       catalog.Location
	copies    int
	bytes     int64
	intervals [][2]time.Time
}

func (a *locAcc) add(r verifyResult) {
	a.copies++
	a.bytes += r.read
	a.intervals = append(a.intervals, [2]time.Time{r.start, r.end})
}

func (a *locAcc) stats() LocationStats {
	sort.Slice(a.intervals, func(i, j int) bool { return a.intervals[i][0].Before(a.intervals[j][0]) })
	var wall time.Duration
	var cur [2]time.Time
	for i, iv := range a.intervals {
		if i == 0 || iv[0].After(cur[1]) {
			if i > 0 {
				wall += cur[1].Sub(cur[0])
			}
			cur = iv
			continue
		}
		if iv[1].After(cur[1]) {
			cur[1] = iv[1]
		}
	}
	if len(a.intervals) > 0 {
		wall += cur[1].Sub(cur[0])
	}
	st := LocationStats{Location: a.loc.Name, Kind: string(a.loc.Kind), Copies: a.copies, BytesRead: a.bytes, Wall: wall}
	if wall > 0 {
		st.MiBPerSecond = float64(a.bytes) / (1 << 20) / wall.Seconds()
	}
	return st
}

// bytesToRead is what checking one copy costs: the whole file with Full,
// otherwise the two 1 MiB extents of the sparse identity for video and
// audio and nothing (a stat) for stills.
func bytesToRead(a catalog.Asset, full bool) int64 {
	switch {
	case full:
		return a.Size
	case a.Kind.UsesSparseID():
		return min(a.Size, 2*identity.Chunk)
	default:
		return 0
	}
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
