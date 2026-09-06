package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scottlaird/mediamanager/catalog"
	"github.com/scottlaird/mediamanager/config"
	"github.com/scottlaird/mediamanager/identity"
	"github.com/scottlaird/mediamanager/media"
	"github.com/scottlaird/mediamanager/naming"
	"github.com/scottlaird/mediamanager/scan"
)

// AdoptMode says what Adopt may do to files it finds.
type AdoptMode int

const (
	// InPlace catalogues files under the names and paths they already
	// have. Nothing on the location is written.
	InPlace AdoptMode = iota
	// Migrate renames files into the tool's own layout and naming, on the
	// same volume, never copying and never overwriting. This is the one
	// deliberate exception to the rule that the NAS is append-only; it is
	// meant as a one-off when bringing an existing library under
	// management.
	Migrate
)

func (m AdoptMode) String() string {
	if m == Migrate {
		return "migrate"
	}
	return "in-place"
}

// AdoptOptions tune Adopt.
type AdoptOptions struct {
	Mode AdoptMode
	// DryRun computes the plan and reports it without cataloguing or
	// renaming anything. Run it first.
	DryRun bool
}

// AdoptAction is one line of the plan.
type AdoptAction struct {
	// Op is one of: adopt, rename, known, duplicate, companion, orphan,
	// unrecognised, unrouted, conflict, empty-dir.
	Op string
	// From and To are paths relative to the location root; To is set for
	// renames and for companions that move.
	From    string
	To      string
	AssetID string
	Note    string
}

// AdoptReport is the plan and, unless DryRun, what was done.
type AdoptReport struct {
	Location string
	Mode     AdoptMode
	DryRun   bool
	Actions  []AdoptAction
	// Counts by Op.
	Counts map[string]int
	Bytes  int64
}

// legacyName matches the earlier importer's scheme:
// ORIG_YYYY-MM-DD-HHMMSS_hex16. The hash is opaque here.
var legacyName = regexp.MustCompile(`^(.+)_(\d{4}-\d{2}-\d{2}-\d{6})_[0-9a-f]{16}$`)

// Adopt brings files already sitting on a spool or NAS under management.
// Each configured tree's subdirectory under the location is scanned; every
// original gets an identity and a catalog row with a complete copy on this
// location, and its companions are recorded alongside. Duplicate content
// is reported and left where it is; so is anything unrecognised. See
// AdoptMode for what Migrate adds.
func (e *Env) Adopt(ctx context.Context, locationName string, opts AdoptOptions) (*AdoptReport, error) {
	e.init()
	ps, err := e.places(ctx)
	if err != nil {
		return nil, err
	}
	var loc *place
	for i := range ps {
		if ps[i].cat.Name == locationName {
			loc = &ps[i]
		}
	}
	if loc == nil {
		return nil, fmt.Errorf("ingest: no location named %q", locationName)
	}
	if !loc.mounted {
		return nil, fmt.Errorf("ingest: location %q is not mounted", locationName)
	}
	rep := &AdoptReport{Location: locationName, Mode: opts.Mode, DryRun: opts.DryRun, Counts: map[string]int{}}
	ad := &adopter{Env: e, ctx: ctx, loc: *loc, opts: opts, rep: rep, seen: map[string]string{}, claimed: map[string]bool{}, vacated: map[string]bool{}}

	for _, subdir := range e.treeSubdirs() {
		root := filepath.Join(loc.root, subdir)
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			continue
		}
		if err := ad.walk(subdir, root); err != nil {
			return rep, err
		}
	}
	if !opts.DryRun {
		if _, err := e.Reconcile(ctx, ps); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// treeSubdirs lists the distinct subdirectories trees live in, so a
// shared subdir is walked once.
func (e *Env) treeSubdirs() []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range allKinds {
		if t, ok := e.Config.Tree(k); ok && !seen[t.Subdir] {
			seen[t.Subdir] = true
			out = append(out, t.Subdir)
		}
	}
	return out
}

type adopter struct {
	*Env
	ctx  context.Context
	loc  place
	opts AdoptOptions
	rep  *AdoptReport
	// seen maps identity to the location-relative path that claimed it
	// this run, for duplicate reporting.
	seen map[string]string
	// claimed marks destination paths taken this run, so a dry run plans
	// the same suffixes a real run would apply.
	claimed map[string]bool
	// vacated are directories files were moved out of, candidates for
	// removal once empty. Directories the migration never touched, such
	// as hand-made label folders, are left alone.
	vacated map[string]bool
}

// planned is an original that will be (or was) adopted, kept for pairing
// companions after the fact.
type planned struct {
	file  scan.File
	asset catalog.Asset
	// dstRel is the asset's final location-relative path.
	dstRel string
}

func (ad *adopter) act(op, from, to, assetID, note string) {
	ad.rep.Actions = append(ad.rep.Actions, AdoptAction{Op: op, From: from, To: to, AssetID: assetID, Note: note})
	ad.rep.Counts[op]++
}

func (ad *adopter) walk(subdir, root string) error {
	res, err := scan.Scan(root, ad.classifier)
	if err != nil {
		return err
	}
	locRel := func(rel string) string { return path.Join(subdir, rel) }
	for _, rel := range res.Unrecognised {
		ad.act("unrecognised", locRel(rel), "", "", "left alone")
	}
	// Shallower paths first, so when the same content exists at the top of
	// a day directory and again inside an accidental nested copy, the one
	// at the top is the asset and the nested one is the duplicate.
	originals := make([]scan.File, 0, len(res.Files))
	for _, f := range res.Files {
		if f.IsOriginal() {
			originals = append(originals, f)
		}
	}
	sort.SliceStable(originals, func(i, j int) bool {
		di, dj := strings.Count(originals[i].Rel, "/"), strings.Count(originals[j].Rel, "/")
		if di != dj {
			return di < dj
		}
		return originals[i].Rel < originals[j].Rel
	})
	byKey := map[string][]planned{}
	for _, f := range originals {
		if err := ad.ctx.Err(); err != nil {
			return err
		}
		p, ok, err := ad.original(subdir, f)
		if err != nil {
			return fmt.Errorf("%s: %w", locRel(f.Rel), err)
		}
		if ok {
			k := path.Join(f.OriginalDir(), f.Base())
			byKey[k] = append(byKey[k], p)
		}
	}
	for _, f := range res.Files {
		if f.IsOriginal() {
			continue
		}
		cands := byKey[path.Join(f.OriginalDir(), f.Base())]
		var assets []catalog.Asset
		for _, c := range cands {
			if f.Role != scan.Proxy || c.asset.Kind == media.Video {
				assets = append(assets, c.asset)
			}
		}
		if len(assets) == 0 {
			ad.act("orphan", locRel(f.Rel), "", "", "no original beside it")
			continue
		}
		owner := preferredOwner(assets, f.Ext)
		var p planned
		for _, c := range cands {
			if c.asset.ID == owner.ID {
				p = c
			}
		}
		if err := ad.companion(subdir, f, p); err != nil {
			return fmt.Errorf("%s: %w", locRel(f.Rel), err)
		}
	}
	if ad.opts.Mode == Migrate && !ad.opts.DryRun {
		ad.pruneVacated(root)
	}
	ad.emptyDirs(subdir, root)
	return nil
}

// original plans and, unless dry-running, performs the adoption of one
// original. ok is false when the file was not adopted (duplicate,
// conflict, unrouted).
func (ad *adopter) original(subdir string, f scan.File) (planned, bool, error) {
	from := path.Join(subdir, f.Rel)
	tree, ok := ad.Config.Tree(f.Kind)
	if !ok {
		ad.act("unrouted", from, "", "", fmt.Sprintf("no %s tree configured", f.Kind))
		return planned{}, false, nil
	}
	a := catalog.Asset{Kind: f.Kind, Size: f.Size}
	if f.Kind.UsesSparseID() {
		id, _, err := identity.SparseFile(f.Abs)
		if err != nil {
			return planned{}, false, err
		}
		a.ID, a.Scheme = string(id), identity.Scheme
	} else {
		sum, err := identity.FullFile(f.Abs)
		if err != nil {
			return planned{}, false, err
		}
		a.ID, a.Scheme, a.FullSHA256 = sum, "sha256", sum
	}
	if first, dup := ad.seen[a.ID]; dup {
		ad.act("duplicate", from, "", a.ID, "same content as "+first)
		return planned{}, false, nil
	}
	ad.seen[a.ID] = from

	origBase, legacyTime := splitLegacy(f.Base(), ad.Env.loc)
	a.OrigName = origBase
	if f.Ext != "" {
		a.OrigName += "." + f.Ext
	}
	a.CaptureTime = ad.captureTime(f, legacyTime)

	// Already catalogued, perhaps from a card: attach this file as the copy
	// on this location rather than making a second asset.
	known, err := ad.Catalog.Asset(ad.ctx, a.ID)
	switch {
	case err == nil:
		a = known
	case !errors.Is(err, catalog.ErrNotFound):
		return planned{}, false, err
	}
	if err == nil {
		if copies, _ := ad.Catalog.Copies(ad.ctx, a.ID); hasComplete(copies, ad.loc.cat.ID) {
			ad.act("known", from, "", a.ID, "already catalogued here")
			return planned{file: f, asset: a, dstRel: from}, true, nil
		}
	}

	var dstRel string
	switch ad.opts.Mode {
	case InPlace:
		if a.RelPath == "" {
			a.RelPath = f.Rel
		}
		if other, err := ad.Catalog.AssetByPath(ad.ctx, a.Kind, a.RelPath); err == nil && other.ID != a.ID {
			ad.act("conflict", from, "", a.ID, "another asset already owns "+a.RelPath+"; migrate to resolve")
			return planned{}, false, nil
		}
		dstRel = from
		ad.act("adopt", from, "", a.ID, "")
	case Migrate:
		rel, err := ad.migratePath(a, tree, from)
		if err != nil {
			return planned{}, false, err
		}
		a.RelPath = rel
		dstRel = path.Join(tree.Subdir, rel)
		if dstRel == from {
			ad.act("adopt", from, "", a.ID, "already in place")
		} else {
			ad.act("rename", from, dstRel, a.ID, "")
		}
	}
	ad.claimed[dstRel] = true
	ad.rep.Bytes += f.Size
	if ad.opts.DryRun {
		return planned{file: f, asset: a, dstRel: dstRel}, true, nil
	}

	if dstRel != from {
		if err := ad.rename(from, dstRel); err != nil {
			return planned{}, false, err
		}
	}
	if err := ad.Catalog.PutAsset(ad.ctx, a); err != nil {
		return planned{}, false, err
	}
	cp := catalog.Copy{AssetID: a.ID, LocationID: ad.loc.cat.ID, RelPath: dstRel, State: catalog.Complete, VerifiedAt: time.Now(), FullSHA256: a.FullSHA256}
	if err := ad.Catalog.PutCopy(ad.ctx, cp); err != nil {
		return planned{}, false, err
	}
	return planned{file: f, asset: a, dstRel: dstRel}, true, nil
}

// migratePath picks the canonical relpath for an asset, stepping past
// names owned by other assets or already claimed this run.
func (ad *adopter) migratePath(a catalog.Asset, tree config.Tree, from string) (string, error) {
	if a.RelPath != "" {
		return a.RelPath, nil
	}
	rel, err := naming.DateScheme{}.Path(naming.Asset{Kind: a.Kind, OrigName: a.OrigName, ID: identity.ID(a.ID), CaptureTime: a.CaptureTime})
	if err != nil {
		return "", err
	}
	for n := 0; n < 100; n++ {
		try := rel
		if n > 0 {
			try = naming.WithSuffix(rel, n)
		}
		dst := path.Join(tree.Subdir, try)
		if dst == from {
			return try, nil
		}
		if ad.claimed[dst] {
			continue
		}
		if other, err := ad.Catalog.AssetByPath(ad.ctx, a.Kind, try); err == nil && other.ID != a.ID {
			continue
		}
		if _, err := os.Lstat(abs(ad.loc.root, dst)); err == nil {
			continue
		}
		return try, nil
	}
	return "", fmt.Errorf("no free name near %s", rel)
}

// companion records (and in Migrate mode moves) a proxy or sidecar next to
// its adopted original.
func (ad *adopter) companion(subdir string, f scan.File, p planned) error {
	from := path.Join(subdir, f.Rel)
	role := catalog.Role(f.Role)
	tree, _ := ad.Config.Tree(p.asset.Kind)
	to := from
	if ad.opts.Mode == Migrate {
		to = path.Join(tree.Subdir, companionPath(p.asset.RelPath, role, f.Ext))
	}
	if to != from {
		ad.act("companion", from, to, p.asset.ID, string(role))
	} else {
		ad.act("companion", from, "", p.asset.ID, string(role))
	}
	if ad.opts.DryRun {
		return nil
	}
	if to != from {
		if err := ad.rename(from, to); err != nil {
			return err
		}
	}
	sum, err := identity.FullFile(abs(ad.loc.root, to))
	if err != nil {
		return err
	}
	return ad.Catalog.PutCompanion(ad.ctx, catalog.Companion{AssetID: p.asset.ID, LocationID: ad.loc.cat.ID, Role: role, Ext: f.Ext, RelPath: to, SHA256: sum})
}

// rename moves a file within the location, refusing to overwrite.
func (ad *adopter) rename(from, to string) error {
	src, dst := abs(ad.loc.root, from), abs(ad.loc.root, to)
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("refusing to overwrite %s", to)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	ad.logf("rename %s -> %s", from, to)
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	ad.vacated[filepath.Dir(src)] = true
	return nil
}

// captureTime decides when an adopted file was shot. The embedded legacy
// timestamp beats file metadata; a YYYY/MM/DD directory beats both for
// the date, because it records where a person filed it.
func (ad *adopter) captureTime(f scan.File, legacy time.Time) time.Time {
	t := legacy
	if t.IsZero() {
		t, _ = scan.CaptureTime(f, ad.Env.loc)
	}
	y, m, d, depth := dirDate(f.Rel)
	switch depth {
	case 3:
		return time.Date(y, m, d, t.Hour(), t.Minute(), t.Second(), 0, t.Location())
	case 2:
		if t.Year() != y || t.Month() != m {
			ad.act("date-mismatch", "", "", "", fmt.Sprintf("%s is filed under %04d/%02d but was shot %s", f.Rel, y, m, t.Format("2006-01-02")))
		}
	}
	return t
}

// dirDate reads leading YYYY/MM/DD components off a relative path and
// reports how many it found.
func dirDate(rel string) (y int, m time.Month, d int, depth int) {
	parts := strings.Split(rel, "/")
	num := func(i, width int) (int, bool) {
		if i >= len(parts)-1 || len(parts[i]) != width {
			return 0, false
		}
		n, err := strconv.Atoi(parts[i])
		return n, err == nil
	}
	yy, ok := num(0, 4)
	if !ok || yy < 1990 || yy > 2100 {
		return 0, 0, 0, 0
	}
	mm, ok := num(1, 2)
	if !ok || mm < 1 || mm > 12 {
		return yy, 0, 0, 1
	}
	dd, ok := num(2, 2)
	if !ok || dd < 1 || dd > 31 {
		return yy, time.Month(mm), 0, 2
	}
	return yy, time.Month(mm), dd, 3
}

// splitLegacy strips the earlier importer's _YYYY-MM-DD-HHMMSS_hex16
// suffix, returning the camera's name and the embedded time (zero when
// the name is not in that form).
func splitLegacy(base string, loc *time.Location) (string, time.Time) {
	m := legacyName.FindStringSubmatch(base)
	if m == nil {
		return base, time.Time{}
	}
	t, err := time.ParseInLocation("2006-01-02-150405", m[2], loc)
	if err != nil {
		return base, time.Time{}
	}
	return m[1], t
}

// emptyDirs reports directories holding nothing but junk. In the existing
// library these are mostly hand-made labels like 20250129-leavenworth,
// which is project-name information worth keeping in view.
func (ad *adopter) emptyDirs(subdir, root string) {
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == root {
			return nil
		}
		if scan.IsJunk(d.Name()) {
			return filepath.SkipDir
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if !scan.IsJunk(e.Name()) {
				return nil
			}
		}
		rel, _ := filepath.Rel(root, p)
		ad.act("empty-dir", path.Join(subdir, filepath.ToSlash(rel)), "", "", "")
		return filepath.SkipDir
	})
}

// pruneVacated removes directories the migration emptied, walking up
// toward root while removals succeed. Anything still holding a file, junk
// included, stays, and so does every directory nothing was moved out of.
func (ad *adopter) pruneVacated(root string) {
	dirs := make([]string, 0, len(ad.vacated))
	for d := range ad.vacated {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		for d != root && strings.HasPrefix(d, root) {
			if err := os.Remove(d); err != nil {
				break
			}
			d = filepath.Dir(d)
		}
	}
}
