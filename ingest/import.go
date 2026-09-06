package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/scottlaird/mediamanager/catalog"
	"github.com/scottlaird/mediamanager/copyfile"
	"github.com/scottlaird/mediamanager/linktree"
	"github.com/scottlaird/mediamanager/media"
)

// Failure is one asset that did not finish a step.
type Failure struct {
	AssetID string
	Step    string
	Err     error
}

// Summary is the outcome of an Import.
type Summary struct {
	Source   *Source
	Spooled  int
	Archived int
	// SpoolFull lists assets that skipped the spool for lack of room and
	// were archived straight from the source instead.
	SpoolFull []string
	Failures  []Failure
	// SafeToFormat is true when every asset on the source has a complete
	// NAS copy. It is a report; the tool never erases a source (rule R6).
	SafeToFormat bool
}

// Import runs the whole flow for one source: audit the link trees,
// register what is on the card, link it so it is editable at once, then
// spool each asset in turn (one reader per source) and archive spooled
// assets to the NAS as they complete. It finishes by archiving anything
// left over from earlier runs whose copy is reachable. Per-asset problems
// are collected in the Summary; only systemic ones are returned as errors.
func (e *Env) Import(ctx context.Context, sourceRoot string) (*Summary, error) {
	e.init()
	ps, err := e.places(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := e.Reconcile(ctx, ps); err != nil {
		return nil, err
	}
	src, err := e.ScanSource(ctx, sourceRoot)
	if err != nil {
		return nil, err
	}
	sum := &Summary{Source: src}
	e.logf("%s: %d assets, %d new", src.Root, len(src.Assets), len(src.New))
	if _, err := e.Reconcile(ctx, ps); err != nil {
		return nil, err
	}

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		archive = make(chan string)
	)
	fail := func(id, step string, err error) {
		mu.Lock()
		sum.Failures = append(sum.Failures, Failure{id, step, err})
		mu.Unlock()
	}
	// Archivers: bounded by the NAS semaphore inside Archive, so start one
	// goroutine per slot and let the channel feed them.
	for i := 0; i < cap(e.nas); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range archive {
				results, err := e.Archive(ctx, ps, id)
				if err != nil {
					fail(id, "archive", err)
					continue
				}
				mu.Lock()
				for _, r := range results {
					if !r.Skipped {
						sum.Archived++
					}
				}
				mu.Unlock()
			}
		}()
	}

	queued := map[string]bool{}
	for _, id := range src.Assets {
		if ctx.Err() != nil {
			break
		}
		if queued[id] {
			continue // the same content under two names on one card
		}
		queued[id] = true
		done, err := e.archived(ctx, ps, id)
		if err != nil {
			fail(id, "lookup", err)
			continue
		}
		if done {
			archive <- id // no-op for the original; refreshes sidecars on the NAS
			continue
		}
		r, err := e.Spool(ctx, ps, id)
		switch {
		case errors.Is(err, ErrSpoolFull):
			mu.Lock()
			sum.SpoolFull = append(sum.SpoolFull, id)
			mu.Unlock()
			e.logf("%v; archiving from source", err)
		case err != nil:
			fail(id, "spool", err)
			continue
		case !r.Skipped:
			mu.Lock()
			sum.Spooled++
			mu.Unlock()
		}
		archive <- id
	}
	if ctx.Err() == nil {
		leftovers, err := e.Catalog.NeedsArchive(ctx)
		if err == nil {
			for _, a := range leftovers {
				if !queued[a.ID] {
					queued[a.ID] = true
					archive <- a.ID
				}
			}
		}
	}
	close(archive)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return sum, err
	}

	sum.SafeToFormat = len(src.Assets) > 0
	for _, id := range src.Assets {
		done, err := e.archived(ctx, ps, id)
		if err != nil || !done {
			sum.SafeToFormat = false
			break
		}
	}
	return sum, nil
}

// archived reports whether the asset has a complete copy on a mounted NAS.
func (e *Env) archived(ctx context.Context, ps []place, id string) (bool, error) {
	copies, err := e.Catalog.Copies(ctx, id)
	if err != nil {
		return false, err
	}
	for _, cp := range copies {
		if cp.State != catalog.Complete || cp.Location.Kind != catalog.NAS {
			continue
		}
		for _, p := range ps {
			if p.cat.ID == cp.LocationID && p.mounted {
				return true, nil
			}
		}
	}
	return false, nil
}

// ArchiveAll archives every asset that lacks a NAS copy and has a
// reachable complete copy: the "restart the copy" path after an
// interrupted run, with no card involved.
func (e *Env) ArchiveAll(ctx context.Context) (*Summary, error) {
	e.init()
	ps, err := e.places(ctx)
	if err != nil {
		return nil, err
	}
	sum := &Summary{}
	assets, err := e.Catalog.NeedsArchive(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range assets {
		results, err := e.Archive(ctx, ps, a.ID)
		if err != nil {
			sum.Failures = append(sum.Failures, Failure{a.ID, "archive", err})
			continue
		}
		for _, r := range results {
			if !r.Skipped {
				sum.Archived++
			}
		}
	}
	return sum, nil
}

// ArchiveItem is one asset the archive step would act on.
type ArchiveItem struct {
	Asset catalog.Asset
	// From is where the bytes would be read; empty when no complete copy
	// is mounted, in which case Reason says so and nothing would happen.
	From string
	// To lists the NAS locations lacking a complete copy. Resume is bytes
	// already present in a .partial on the first of them.
	To     []string
	Resume int64
	Reason string
}

// ArchivePlan reports what ArchiveAll would do, without doing it: every
// asset lacking a NAS copy, where it would be read from, which NAS
// locations would receive it, and any partial already on disk to resume.
func (e *Env) ArchivePlan(ctx context.Context) ([]ArchiveItem, error) {
	e.init()
	ps, err := e.places(ctx)
	if err != nil {
		return nil, err
	}
	srcRoots, err := e.sourceRoots(ctx)
	if err != nil {
		return nil, err
	}
	rootOf := e.rootOf(ps, srcRoots)
	assets, err := e.Catalog.NeedsArchive(ctx)
	if err != nil {
		return nil, err
	}
	var items []ArchiveItem
	for _, a := range assets {
		it := ArchiveItem{Asset: a}
		copies, err := e.Catalog.Copies(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		if from, ok := linktree.Choose(copies, rootOf); ok {
			it.From = from
		}
		rel, relErr := e.treeRel(a)
		for _, p := range ps {
			if !p.mounted || p.cat.Kind != catalog.NAS || hasComplete(copies, p.cat.ID) {
				continue
			}
			it.To = append(it.To, p.cat.Name)
			if relErr == nil && len(it.To) == 1 {
				if st, err := os.Stat(abs(p.root, rel) + copyfile.PartialSuffix); err == nil {
					it.Resume = st.Size()
				}
			}
		}
		switch {
		case relErr != nil:
			it.Reason = relErr.Error()
		case it.From == "":
			it.Reason = "no complete copy is mounted"
		case len(it.To) == 0:
			it.Reason = "no NAS is mounted"
		}
		items = append(items, it)
	}
	return items, nil
}

// Select resolves user arguments to assets: each is an asset ID, or a
// prefix of relpath or kind/relpath as in ListOptions. Unknown arguments
// are an error rather than silently matching nothing.
func (e *Env) Select(ctx context.Context, args []string) ([]AssetRef, error) {
	var refs []AssetRef
	seen := map[string]bool{}
	for _, arg := range args {
		if a, err := e.Catalog.Asset(ctx, arg); err == nil {
			if !seen[a.ID] {
				seen[a.ID] = true
				refs = append(refs, RefOf(a))
			}
			continue
		}
		matches, err := e.List(ctx, ListOptions{Prefixes: []string{arg}})
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("ingest: %q matches no asset id or path", arg)
		}
		for _, m := range matches {
			if !seen[m.Asset.ID] {
				seen[m.Asset.ID] = true
				refs = append(refs, RefOf(m.Asset))
			}
		}
	}
	return refs, nil
}

// SpoolSummary is the outcome of SpoolAll.
type SpoolSummary struct {
	Spooled  int
	Skipped  int
	Failures []Failure
	Copied   int64
	Duration time.Duration
}

// SpoolAll brings assets onto local storage from wherever their best copy
// is, typically the NAS after a flush, and optionally pins them so the
// next flush leaves them alone: `mm spool`. Copies run one at a time.
func (e *Env) SpoolAll(ctx context.Context, refs []AssetRef, pin bool) (*SpoolSummary, error) {
	e.init()
	ps, err := e.places(ctx)
	if err != nil {
		return nil, err
	}
	sum := &SpoolSummary{}
	for _, ref := range refs {
		if pin {
			if err := e.Catalog.SetPinned(ctx, ref.ID, true); err != nil {
				sum.Failures = append(sum.Failures, Failure{ref.ID, "pin", err})
				continue
			}
		}
		r, err := e.Spool(ctx, ps, ref.ID)
		if err != nil {
			sum.Failures = append(sum.Failures, Failure{ref.ID, "spool", err})
			continue
		}
		if r.Skipped {
			sum.Skipped++
			continue
		}
		sum.Spooled++
		sum.Copied += r.Copied
		sum.Duration += r.Duration
	}
	return sum, nil
}

// SpoolAsset is Spool with location resolution done for the caller.
func (e *Env) SpoolAsset(ctx context.Context, id string) (CopyResult, error) {
	ps, err := e.places(ctx)
	if err != nil {
		return CopyResult{}, err
	}
	return e.Spool(ctx, ps, id)
}

// ArchiveAsset is Archive with location resolution done for the caller.
func (e *Env) ArchiveAsset(ctx context.Context, id string) ([]CopyResult, error) {
	ps, err := e.places(ctx)
	if err != nil {
		return nil, err
	}
	return e.Archive(ctx, ps, id)
}

// IsArchived reports whether the asset has a complete copy on a mounted NAS.
func (e *Env) IsArchived(ctx context.Context, id string) (bool, error) {
	ps, err := e.places(ctx)
	if err != nil {
		return false, err
	}
	return e.archived(ctx, ps, id)
}

// NeedsArchiveRefs lists assets lacking a NAS copy, oldest capture first.
func (e *Env) NeedsArchiveRefs(ctx context.Context) ([]AssetRef, error) {
	assets, err := e.Catalog.NeedsArchive(ctx)
	if err != nil {
		return nil, err
	}
	refs := make([]AssetRef, 0, len(assets))
	for _, a := range assets {
		refs = append(refs, RefOf(a))
	}
	return refs, nil
}

// NeedsArchiveIDs lists assets lacking a NAS copy, oldest capture first.
func (e *Env) NeedsArchiveIDs(ctx context.Context) ([]string, error) {
	assets, err := e.Catalog.NeedsArchive(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(assets))
	for _, a := range assets {
		ids = append(ids, a.ID)
	}
	return ids, nil
}

// FlushSpool is Flush with location resolution done for the caller.
func (e *Env) FlushSpool(ctx context.Context, spoolName string, opts FlushOptions) (FlushReport, error) {
	ps, err := e.places(ctx)
	if err != nil {
		return FlushReport{}, err
	}
	return e.Flush(ctx, ps, spoolName, opts)
}

// Relink resolves locations and reconciles every link tree.
func (e *Env) Relink(ctx context.Context) (ReconcileReport, error) {
	ps, err := e.places(ctx)
	if err != nil {
		return ReconcileReport{}, err
	}
	return e.Reconcile(ctx, ps)
}

// Status is a snapshot for reporting.
type Status struct {
	Locations []LocationStatus
	Assets    []AssetStatus
}

// LocationStatus is one configured location this run.
type LocationStatus struct {
	Name    string
	Kind    string
	Root    string
	Mounted bool
	Free    int64
}

// AssetStatus is one asset and where its complete copies are.
type AssetStatus struct {
	Asset  catalog.Asset
	Copies []string // location names holding a complete copy
	State  string   // discovered, spooled, archived, flushed, unavailable, with +partial when a copy is in flight
	// Detail is filled in by List: every copy row and companion.
	CopyRows   []catalog.Copy
	Companions []catalog.Companion
}

// Status reports every location and asset.
func (e *Env) Status(ctx context.Context) (*Status, error) {
	e.init()
	ps, err := e.places(ctx)
	if err != nil {
		return nil, err
	}
	st := &Status{}
	for _, p := range ps {
		ls := LocationStatus{Name: p.cat.Name, Kind: string(p.cat.Kind), Root: p.root, Mounted: p.mounted}
		if p.mounted {
			ls.Free, _ = freeSpace(p.root)
		}
		st.Locations = append(st.Locations, ls)
	}
	st.Assets, err = e.assetStatuses(ctx, false)
	return st, err
}

// ListOptions filter List. Empty fields match everything.
type ListOptions struct {
	// Prefixes match the start of "kind/relpath" or of relpath alone.
	Prefixes []string
	States   []string
	// Location keeps assets with a complete copy on that location.
	Location string
	Kind     media.Kind
	// Companions loads companion rows for each asset.
	Companions bool
}

// List returns assets matching opts, ordered by relpath then kind, so a
// shared tree reads in directory order.
func (e *Env) List(ctx context.Context, opts ListOptions) ([]AssetStatus, error) {
	e.init()
	all, err := e.assetStatuses(ctx, opts.Companions)
	if err != nil {
		return nil, err
	}
	var out []AssetStatus
	for _, as := range all {
		if opts.matches(as) {
			out = append(out, as)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Asset.RelPath != out[j].Asset.RelPath {
			return out[i].Asset.RelPath < out[j].Asset.RelPath
		}
		return out[i].Asset.Kind < out[j].Asset.Kind
	})
	return out, nil
}

func (o ListOptions) matches(as AssetStatus) bool {
	if o.Kind != media.Unknown && as.Asset.Kind != o.Kind {
		return false
	}
	if len(o.States) > 0 && !slices.Contains(o.States, strings.TrimSuffix(as.State, "+partial")) && !slices.Contains(o.States, as.State) {
		return false
	}
	if o.Location != "" && !slices.Contains(as.Copies, o.Location) {
		return false
	}
	if len(o.Prefixes) > 0 {
		full := as.Asset.Kind.String() + "/" + as.Asset.RelPath
		ok := false
		for _, p := range o.Prefixes {
			p = strings.TrimPrefix(strings.TrimSuffix(p, "/"), "./")
			if p == "" || strings.HasPrefix(as.Asset.RelPath, p) || strings.HasPrefix(full, p) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func (e *Env) assetStatuses(ctx context.Context, withCompanions bool) ([]AssetStatus, error) {
	var out []AssetStatus
	for _, kind := range allKinds {
		assets, err := e.Catalog.AssetsByKind(ctx, kind)
		if err != nil {
			return nil, err
		}
		for _, a := range assets {
			copies, err := e.Catalog.Copies(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			as := assetStatus(a, copies)
			if withCompanions {
				if as.Companions, err = e.Catalog.Companions(ctx, a.ID); err != nil {
					return nil, err
				}
			}
			out = append(out, as)
		}
	}
	return out, nil
}

func assetStatus(a catalog.Asset, copies []catalog.Copy) AssetStatus {
	as := AssetStatus{Asset: a, CopyRows: copies}
	var spool, nas, source, partial bool
	for _, cp := range copies {
		if cp.State != catalog.Complete {
			partial = true
			continue
		}
		as.Copies = append(as.Copies, cp.Location.Name)
		switch cp.Location.Kind {
		case catalog.Spool:
			spool = true
		case catalog.NAS:
			nas = true
		case catalog.Source:
			source = true
		}
	}
	switch {
	case nas && spool:
		as.State = "archived"
	case nas:
		as.State = "flushed"
	case spool:
		as.State = "spooled"
	case source:
		as.State = "discovered"
	default:
		as.State = "unavailable"
	}
	if partial {
		as.State += "+partial"
	}
	return as
}
