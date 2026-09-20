package ingest

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/scottlaird/mediamanager/catalog"
	"github.com/scottlaird/mediamanager/media"
	"github.com/scottlaird/mediamanager/naming"
	"github.com/scottlaird/mediamanager/scan"
)

// PlanItem is one file on a source and what Import would do with it.
type PlanItem struct {
	Rel  string
	Kind media.Kind
	Role scan.Role
	Size int64
	// Action is one of: new, known, companion, orphan, unrouted, unrecognised.
	Action string
	// Dest is where a new original would land, relative to its tree, with
	// the identity part of the name left as "<id>" since computing it
	// means reading the file.
	Dest string
	// CaptureTime and TimeSource say when the file was shot and how that
	// was determined.
	CaptureTime time.Time
	TimeSource  string
	Note        string
}

// ImportPlan is what a dry run reports.
type ImportPlan struct {
	Root  string
	Shape string
	// Source is the catalog location the card would be recorded as, if it
	// has been seen before; empty otherwise.
	Source string
	Items  []PlanItem
	Counts map[string]int
	// NewBytes is the size of everything that would be copied.
	NewBytes int64
	// Spool names the spool a new asset would be copied to first, or is
	// empty with SpoolNote saying why not.
	Spool     string
	SpoolNote string
	NAS       []string
}

// Plan reports what Import would do with a source without doing any of
// it: no identity is computed, nothing is written to the catalog or any
// tier. Files already seen are recognised through the source-file cache
// (path, size and mtime), so a re-inserted card is mostly "known".
func (e *Env) Plan(ctx context.Context, root string) (*ImportPlan, error) {
	e.init()
	root = filepath.Clean(root)
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("ingest: source %s is not a directory", root)
	}
	res, err := scan.Scan(root, e.classifier)
	if err != nil {
		return nil, err
	}
	plan := &ImportPlan{Root: root, Shape: res.Shape.String(), Counts: map[string]int{}}

	// The location this card would be recorded as, without recording it.
	var loc *catalog.Location
	if l, err := e.sourceLocationLookup(ctx, root); err == nil {
		loc = &l
		plan.Source = l.Name
	}

	ps, err := e.places(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if !p.mounted {
			continue
		}
		switch p.cat.Kind {
		case catalog.Spool:
			if plan.Spool == "" {
				plan.Spool = p.cat.Name
			}
		case catalog.NAS:
			plan.NAS = append(plan.NAS, p.cat.Name)
		}
	}
	if plan.Spool == "" {
		plan.SpoolNote = "no spool is mounted; new assets would be archived straight from the source"
	}

	byKey := map[string]bool{} // originals present, for companion pairing
	for _, f := range res.Files {
		if !f.IsOriginal() {
			continue
		}
		it := PlanItem{Rel: f.Rel, Kind: f.Kind, Role: f.Role, Size: f.Size}
		tree, ok := e.Config.Tree(f.Kind)
		if !ok {
			it.Action, it.Note = "unrouted", fmt.Sprintf("no %s tree configured", f.Kind)
			plan.add(it)
			continue
		}
		byKey[path.Join(f.OriginalDir(), f.Base())] = true
		known := false
		if loc != nil {
			if id, ok, err := e.Catalog.LookupSourceFile(ctx, loc.ID, f.Rel, f.Size, f.ModTime); err == nil && ok {
				if a, err := e.Catalog.Asset(ctx, id); err == nil {
					it.Action, it.Dest = "known", path.Join(tree.Subdir, a.RelPath)
					it.CaptureTime = a.CaptureTime
					known = true
				}
			}
		}
		if !known {
			t, src := scan.CaptureTime(f, e.loc)
			it.CaptureTime, it.TimeSource = t, src.String()
			it.Action = "new"
			it.Dest = path.Join(tree.Subdir, plannedPath(f, t))
			plan.NewBytes += f.Size
		}
		plan.add(it)
	}
	for _, f := range res.Files {
		if f.IsOriginal() {
			continue
		}
		it := PlanItem{Rel: f.Rel, Kind: f.Kind, Role: f.Role, Size: f.Size, Action: "companion", Note: string(f.Role)}
		if !byKey[path.Join(f.OriginalDir(), f.Base())] {
			it.Action, it.Note = "orphan", "no original beside it"
		}
		plan.add(it)
	}
	for _, rel := range res.Unrecognised {
		plan.add(PlanItem{Rel: rel, Action: "unrecognised"})
	}
	sort.SliceStable(plan.Items, func(i, j int) bool { return plan.Items[i].Rel < plan.Items[j].Rel })
	return plan, nil
}

func (p *ImportPlan) add(it PlanItem) {
	p.Items = append(p.Items, it)
	p.Counts[it.Action]++
}

// plannedPath is the permanent path a new file would get, with the
// identity left symbolic.
func plannedPath(f scan.File, t time.Time) string {
	na := naming.Asset{Kind: f.Kind, OrigName: f.Name(), CaptureTime: t}
	if f.Kind.UsesSparseID() {
		na.ID = "0000000000000000"
	}
	rel, err := naming.DateScheme{}.Path(na)
	if err != nil {
		return "?"
	}
	if f.Kind.UsesSparseID() {
		rel = replaceLast(rel, "-0000000000000000.", "-<id>.")
	}
	return rel
}

func replaceLast(s, old, new string) string {
	i := len(s) - 1
	for ; i >= 0; i-- {
		if i+len(old) <= len(s) && s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	return s
}

// sourceLocationLookup is sourceLocation without the upsert: the name the
// card would get, resolved to an existing catalog row if there is one.
func (e *Env) sourceLocationLookup(ctx context.Context, root string) (catalog.Location, error) {
	name := "source:" + root
	if e.Identify != nil {
		if info, err := e.Identify(root); err == nil {
			switch {
			case info.UUID != "":
				name = "source:" + info.UUID
			case info.Label != "":
				name = "source:" + info.Label
			}
		}
	}
	return e.Catalog.LocationByName(ctx, name)
}
