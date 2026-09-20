// Package ingest runs the media workflows: import from a source, spool,
// archive to the NAS, flush spools, and keep the link tree current.
//
// Every step is written to be idempotent and resumable, with plain
// arguments and results, so that phase 2 can register each one as a
// Temporal activity unchanged. The phase 1 driver in Import is a small
// in-process orchestrator over the same steps.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scottlaird/mediamanager/catalog"
	"github.com/scottlaird/mediamanager/config"
	"github.com/scottlaird/mediamanager/media"
	"github.com/scottlaird/mediamanager/scan"
	"github.com/scottlaird/mediamanager/volume"
)

// Env is everything the steps need. Build one per process.
type Env struct {
	Config  *config.Config
	Catalog *catalog.DB
	// Find resolves a volume UUID to a mount point; volume.Find in
	// production, nil in tests that use absolute paths only.
	Find func(uuid string) (string, error)
	// Identify learns the volume behind a path; volume.Identify in
	// production. Optional: without it sources are named by path.
	Identify func(path string) (volume.Info, error)
	// Logf receives progress lines. Optional.
	Logf func(format string, args ...any)

	loc        *time.Location
	classifier *scan.Classifier

	mu  sync.Mutex // serialises reconcile and location resolution
	nas chan struct{}
	// nasImport bounds archive copies while spooling; nil means none.
	nasImport chan struct{}
	once      sync.Once

	assetMu sync.Mutex
	assets  map[string]*sync.Mutex
	copySeq atomic.Int64

	// spooling counts spool copies in progress in this process; archive
	// copies are capped while it is non-zero.
	spooling atomic.Int32

	identMu    sync.Mutex
	identCache map[string]identEntry
}

type identEntry struct {
	info volume.Info
	err  error
	at   time.Time
}

// identifyTTL bounds how long a volume lookup is reused. Identify shells
// out to diskutil on macOS, which is far too slow to run per copy.
const identifyTTL = 30 * time.Second

// identify is Identify with a short cache.
func (e *Env) identify(path string) (volume.Info, error) {
	if e.Identify == nil {
		return volume.Info{}, errors.New("no identify")
	}
	e.identMu.Lock()
	defer e.identMu.Unlock()
	if ent, ok := e.identCache[path]; ok && time.Since(ent.at) < identifyTTL {
		return ent.info, ent.err
	}
	if e.identCache == nil {
		e.identCache = map[string]identEntry{}
	}
	info, err := e.Identify(path)
	e.identCache[path] = identEntry{info, err, time.Now()}
	return info, err
}

// archiveSlot admits one archive copy under the import-time cap: while
// spool copies are running in this process, at most NASWhileImporting
// archive copies proceed (none, if it is 0). The wait keeps the caller's
// progress callback, and so any activity heartbeat, alive. The returned
// func releases the slot.
func (e *Env) archiveSlot(ctx context.Context, total int64) (release func(), err error) {
	e.init()
	report := progressFrom(ctx)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	logged := false
	for {
		if e.spooling.Load() == 0 {
			return func() {}, nil
		}
		if e.nasImport != nil {
			select {
			case e.nasImport <- struct{}{}:
				return func() { <-e.nasImport }, nil
			default:
			}
		}
		if !logged {
			e.logf("archive waiting: %d spool copies in progress, %d archive copies allowed meanwhile", e.spooling.Load(), cap(e.nasImport))
			logged = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
			if report != nil {
				report(0, total)
			}
		}
	}
}

// lockAsset serialises tier transitions of one asset, so two concurrent
// steps never write the same .partial. It returns the unlock function.
func (e *Env) lockAsset(id string) func() {
	e.assetMu.Lock()
	if e.assets == nil {
		e.assets = map[string]*sync.Mutex{}
	}
	m, ok := e.assets[id]
	if !ok {
		m = &sync.Mutex{}
		e.assets[id] = m
	}
	e.assetMu.Unlock()
	m.Lock()
	return m.Unlock
}

func (e *Env) init() {
	e.once.Do(func() {
		e.loc, _ = e.Config.Location()
		if e.loc == nil {
			e.loc = time.Local
		}
		e.classifier = e.Config.Classifier()
		n := e.Config.Concurrency.NAS
		if n <= 0 {
			n = 1
		}
		e.nas = make(chan struct{}, n)
		if k := e.Config.Concurrency.NASWhileImporting(); k > 0 {
			e.nasImport = make(chan struct{}, k)
		}
	})
}

func (e *Env) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// place is a configured spool or NAS as it stands this run.
type place struct {
	cfg     config.Location
	cat     catalog.Location
	root    string
	mounted bool
}

// places resolves every configured location, recording each in the
// catalog and learning volume UUIDs where it can. A location whose volume
// is absent or whose root directory is missing is returned unmounted and
// is skipped by every step; it is never treated as empty.
func (e *Env) places(ctx context.Context) ([]place, error) {
	e.init()
	var out []place
	for _, l := range e.Config.Locations {
		p := place{cfg: l}
		root, err := l.Root(e.Find)
		switch {
		case err == nil:
			if st, serr := os.Stat(root); serr == nil && st.IsDir() {
				p.root, p.mounted = root, true
			}
		case errors.Is(err, config.ErrNotMounted), errors.Is(err, volume.ErrNotMounted):
		default:
			return nil, err
		}
		cat := catalog.Location{Kind: catalog.LocationKind(l.Kind), Name: l.Name, VolumeUUID: l.VolumeUUID, Priority: l.Priority, Root: p.root}
		if p.mounted && cat.VolumeUUID == "" && e.Identify != nil {
			if info, err := e.identify(p.root); err == nil {
				cat.VolumeUUID, cat.Label = info.UUID, info.Label
			}
		}
		if prev, err := e.Catalog.LocationByName(ctx, l.Name); err == nil {
			if cat.VolumeUUID == "" {
				cat.VolumeUUID, cat.Label = prev.VolumeUUID, prev.Label
			} else if prev.VolumeUUID != "" && prev.VolumeUUID != cat.VolumeUUID {
				e.logf("warning: location %s is now volume %s, was %s", l.Name, cat.VolumeUUID, prev.VolumeUUID)
			}
			if !p.mounted {
				cat.Root = prev.Root
			}
		}
		if p.cat, err = e.Catalog.UpsertLocation(ctx, cat); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// rootOf builds the resolver linktree.Choose and the copy steps use: the
// current root of any mounted location, including sources seen this run.
func (e *Env) rootOf(ps []place, sources map[int64]string) func(catalog.Location) (string, bool) {
	roots := map[int64]string{}
	for _, p := range ps {
		if p.mounted {
			roots[p.cat.ID] = p.root
		}
	}
	for id, root := range sources {
		roots[id] = root
	}
	return func(l catalog.Location) (string, bool) {
		r, ok := roots[l.ID]
		return r, ok
	}
}

// treeRel is an asset's path relative to a location root.
func (e *Env) treeRel(a catalog.Asset) (string, error) {
	t, ok := e.Config.Tree(a.Kind)
	if !ok {
		return "", fmt.Errorf("no %s tree configured", a.Kind)
	}
	return path.Join(t.Subdir, a.RelPath), nil
}

// allKinds is every kind in the order trees are reconciled.
var allKinds = []media.Kind{media.Video, media.Audio, media.Still}

// linkRoots lists the distinct link tree roots, in kind order. Two kinds
// configured with the same link share one root.
func (e *Env) linkRoots() []string {
	var roots []string
	seen := map[string]bool{}
	for _, k := range allKinds {
		t, ok := e.Config.Tree(k)
		if !ok || seen[t.Link] {
			continue
		}
		seen[t.Link] = true
		roots = append(roots, t.Link)
	}
	return roots
}

// kindsLinkedAt lists the kinds whose link tree is root.
func (e *Env) kindsLinkedAt(root string) []media.Kind {
	var kinds []media.Kind
	for _, k := range allKinds {
		if t, ok := e.Config.Tree(k); ok && t.Link == root {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

func abs(root, rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }

// freeSpace is a variable so tests can simulate a full spool.
var freeSpace = statfsFree

type progressKey struct{}

// WithProgress attaches a callback that copy steps invoke with bytes done
// and total as they run, in addition to logging. Temporal activities use
// it to heartbeat.
func WithProgress(ctx context.Context, fn func(done, total int64)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

func progressFrom(ctx context.Context) func(done, total int64) {
	fn, _ := ctx.Value(progressKey{}).(func(done, total int64))
	return fn
}
