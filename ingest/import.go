package ingest

import (
	"context"
	"errors"
	"sync"

	"github.com/scottlaird/mediamanager/catalog"
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
	State  string   // discovered, spooled, archived, flushed, partial
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
	for _, kind := range []media.Kind{media.Video, media.Audio, media.Still} {
		assets, err := e.Catalog.AssetsByKind(ctx, kind)
		if err != nil {
			return nil, err
		}
		for _, a := range assets {
			copies, err := e.Catalog.Copies(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			st.Assets = append(st.Assets, assetStatus(a, copies))
		}
	}
	return st, nil
}

func assetStatus(a catalog.Asset, copies []catalog.Copy) AssetStatus {
	as := AssetStatus{Asset: a}
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
