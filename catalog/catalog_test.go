package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/scottlaird/mediamanager/media"
)

var (
	ctx  = context.Background()
	shot = time.Date(2026, 9, 5, 22, 14, 0, 0, time.FixedZone("PDT", -7*3600))
)

func open(t *testing.T) *DB {
	t.Helper()
	c, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func loc(t *testing.T, c *DB, kind LocationKind, name string, prio int) Location {
	t.Helper()
	l, err := c.UpsertLocation(ctx, Location{Kind: kind, Name: name, Priority: prio, Root: "/Volumes/" + name})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func video(id, rel string, when time.Time) Asset {
	return Asset{ID: id, Scheme: "s1", Kind: media.Video, Size: 1 << 30, OrigName: "A001.BRAW", CaptureTime: when, RelPath: rel}
}

func TestOpenIsIdempotentAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PutAsset(ctx, video("3f9a1c2e7b4d5a60", "2026/09/05/a001-3f9a1c2e7b4d5a60.braw", shot)); err != nil {
		t.Fatal(err)
	}
	c.Close()

	c, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a, err := c.Asset(ctx, "3f9a1c2e7b4d5a60")
	if err != nil {
		t.Fatal(err)
	}
	if !a.CaptureTime.Equal(shot) {
		t.Errorf("capture time %v, want %v", a.CaptureTime, shot)
	}
	if _, off := a.CaptureTime.Zone(); off != -7*3600 {
		t.Errorf("capture time lost its zone: %v", a.CaptureTime)
	}
}

func TestPutAsset(t *testing.T) {
	c := open(t)
	a := video("3f9a1c2e7b4d5a60", "2026/09/05/a001-3f9a1c2e7b4d5a60.braw", shot)
	if err := c.PutAsset(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := c.PutAsset(ctx, a); err != nil {
		t.Errorf("second identical put: %v", err)
	}
	// Same ID, different path: a bug somewhere, refused.
	b := a
	b.RelPath = "elsewhere.braw"
	if err := c.PutAsset(ctx, b); err == nil {
		t.Error("same id at a different path was accepted")
	}
	// Different ID, same path: the still-name-clash case.
	d := video("0000000000000000", a.RelPath, shot)
	if err := c.PutAsset(ctx, d); !errors.Is(err, ErrPathTaken) {
		t.Errorf("got %v, want ErrPathTaken", err)
	}
	// Same path in another kind's tree is fine.
	s := Asset{ID: "abc", Scheme: "sha256", Kind: media.Still, OrigName: "x.dng", CaptureTime: shot, RelPath: a.RelPath}
	if err := c.PutAsset(ctx, s); err != nil {
		t.Errorf("same relpath in still tree: %v", err)
	}
	got, err := c.AssetByPath(ctx, media.Still, a.RelPath)
	if err != nil || got.ID != "abc" {
		t.Errorf("AssetByPath = %+v, %v", got, err)
	}
	if _, err := c.Asset(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing asset: %v", err)
	}
	if err := c.SetPinned(ctx, "nope", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("update of missing asset: %v", err)
	}
}

func TestCopiesOrderAndDelete(t *testing.T) {
	c := open(t)
	nas := loc(t, c, NAS, "nas", 0)
	fast := loc(t, c, Spool, "fast", 0)
	slow := loc(t, c, Spool, "slow", 1)
	card := loc(t, c, Source, "card-1", 0)
	a := video("3f9a1c2e7b4d5a60", "2026/09/05/a001-3f9a1c2e7b4d5a60.braw", shot)
	if err := c.PutAsset(ctx, a); err != nil {
		t.Fatal(err)
	}
	for _, cp := range []Copy{
		{AssetID: a.ID, LocationID: card.ID, RelPath: "A001.BRAW", State: Complete},
		{AssetID: a.ID, LocationID: nas.ID, RelPath: a.RelPath, State: Complete, VerifiedAt: shot},
		{AssetID: a.ID, LocationID: slow.ID, RelPath: a.RelPath, State: Complete},
		{AssetID: a.ID, LocationID: fast.ID, RelPath: a.RelPath, State: Partial},
	} {
		if err := c.PutCopy(ctx, cp); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.Copies(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, cp := range got {
		order = append(order, cp.Location.Name+":"+string(cp.State))
	}
	want := []string{"fast:partial", "slow:complete", "nas:complete", "card-1:complete"}
	if len(order) != len(want) {
		t.Fatalf("got %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("got %v, want %v", order, want)
		}
	}
	if !got[2].VerifiedAt.Equal(shot) {
		t.Errorf("verified_at round trip: %v", got[2].VerifiedAt)
	}

	// Upsert flips the partial to complete in place.
	if err := c.PutCopy(ctx, Copy{AssetID: a.ID, LocationID: fast.ID, RelPath: a.RelPath, State: Complete}); err != nil {
		t.Fatal(err)
	}
	got, _ = c.Copies(ctx, a.ID)
	if len(got) != 4 || got[0].State != Complete {
		t.Errorf("after upsert: %+v", got)
	}

	if err := c.DeleteCopy(ctx, a.ID, slow.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteCopy(ctx, a.ID, slow.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	got, _ = c.Copies(ctx, a.ID)
	if len(got) != 3 {
		t.Errorf("after delete: %d copies", len(got))
	}
}

func TestWorklists(t *testing.T) {
	c := open(t)
	nas := loc(t, c, NAS, "nas", 0)
	spool := loc(t, c, Spool, "spool", 0)
	card := loc(t, c, Source, "card", 0)

	older := shot.Add(-24 * time.Hour)
	// spooled only, newer
	spooled := video("1111111111111111", "a.braw", shot)
	// spooled only, older
	spooledOld := video("2222222222222222", "b.braw", older)
	// archived
	archived := video("3333333333333333", "c.braw", shot)
	// archived and pinned
	pinned := video("4444444444444444", "d.braw", older)
	// nas copy still partial
	inflight := video("5555555555555555", "e.braw", shot)
	// only on the card
	discovered := video("6666666666666666", "f.braw", shot)
	for _, a := range []Asset{spooled, spooledOld, archived, pinned, inflight, discovered} {
		if err := c.PutAsset(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	c.SetPinned(ctx, pinned.ID, true)
	puts := []Copy{
		{AssetID: spooled.ID, LocationID: spool.ID, State: Complete},
		{AssetID: spooledOld.ID, LocationID: spool.ID, State: Complete},
		{AssetID: archived.ID, LocationID: spool.ID, State: Complete},
		{AssetID: archived.ID, LocationID: nas.ID, State: Complete},
		{AssetID: pinned.ID, LocationID: spool.ID, State: Complete},
		{AssetID: pinned.ID, LocationID: nas.ID, State: Complete},
		{AssetID: inflight.ID, LocationID: spool.ID, State: Complete},
		{AssetID: inflight.ID, LocationID: nas.ID, State: Partial},
		{AssetID: discovered.ID, LocationID: card.ID, State: Complete},
	}
	for _, cp := range puts {
		cp.RelPath = "x"
		if err := c.PutCopy(ctx, cp); err != nil {
			t.Fatal(err)
		}
	}

	ids := func(as []Asset) []string {
		var out []string
		for _, a := range as {
			out = append(out, a.ID)
		}
		return out
	}
	na, err := c.NeedsArchive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantNA := []string{spooledOld.ID, spooled.ID, inflight.ID, discovered.ID}
	if got := ids(na); !equal(got, wantNA) {
		t.Errorf("NeedsArchive = %v, want %v", got, wantNA)
	}
	fl, err := c.Flushable(ctx, spool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(fl); !equal(got, []string{archived.ID}) {
		t.Errorf("Flushable = %v, want [%s]", got, archived.ID)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLocations(t *testing.T) {
	c := open(t)
	loc(t, c, Spool, "slow", 5)
	fast := loc(t, c, Spool, "fast", 1)
	loc(t, c, NAS, "nas", 0)

	spools, err := c.Locations(ctx, Spool)
	if err != nil {
		t.Fatal(err)
	}
	if len(spools) != 2 || spools[0].Name != "fast" {
		t.Errorf("spools = %+v", spools)
	}
	// Upsert by name updates the mount point without changing the ID.
	again, err := c.UpsertLocation(ctx, Location{Kind: Spool, Name: "fast", Priority: 1, Root: "/Volumes/fast 1"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != fast.ID || again.Root != "/Volumes/fast 1" {
		t.Errorf("upsert = %+v, want id %d", again, fast.ID)
	}
	if _, err := c.LocationByName(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing location: %v", err)
	}
}

func TestSourceFileCache(t *testing.T) {
	c := open(t)
	card := loc(t, c, Source, "card", 0)
	a := video("3f9a1c2e7b4d5a60", "a.braw", shot)
	c.PutAsset(ctx, a)
	mt := time.Date(2026, 9, 5, 14, 0, 0, 0, time.UTC)
	if err := c.PutSourceFile(ctx, SourceFile{card.ID, "DCIM/100/A001.BRAW", 1 << 30, mt, a.ID}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
		size int64
		mt   time.Time
		ok   bool
	}{
		{"hit", "DCIM/100/A001.BRAW", 1 << 30, mt, true},
		{"hit in another zone", "DCIM/100/A001.BRAW", 1 << 30, mt.In(time.FixedZone("x", 3600)), true},
		{"size changed", "DCIM/100/A001.BRAW", 1<<30 + 1, mt, false},
		{"mtime changed", "DCIM/100/A001.BRAW", 1 << 30, mt.Add(time.Second), false},
		{"unknown path", "DCIM/100/A002.BRAW", 1 << 30, mt, false},
	}
	for _, tt := range tests {
		id, ok, err := c.LookupSourceFile(ctx, card.ID, tt.path, tt.size, tt.mt)
		if err != nil {
			t.Fatal(err)
		}
		if ok != tt.ok || (ok && id != a.ID) {
			t.Errorf("%s: got %q, %v; want ok=%v", tt.name, id, ok, tt.ok)
		}
	}
}

func TestCompanionsAndImports(t *testing.T) {
	c := open(t)
	spool := loc(t, c, Spool, "spool", 0)
	a := video("3f9a1c2e7b4d5a60", "a.braw", shot)
	c.PutAsset(ctx, a)
	for _, cp := range []Companion{
		{a.ID, spool.ID, RoleProxy, "mp4", "Proxy/a.mp4", "deadbeef"},
		{a.ID, spool.ID, RoleSidecar, "sidecar", "a.sidecar", ""},
		{a.ID, spool.ID, RoleProxySidecar, "sidecar", "Proxy/a.sidecar", ""},
	} {
		if err := c.PutCompanion(ctx, cp); err != nil {
			t.Fatal(err)
		}
	}
	ps, _ := c.Companions(ctx, a.ID)
	if len(ps) != 3 || ps[0].RelPath != "Proxy/a.mp4" || ps[2].Role != RoleSidecar {
		t.Errorf("companions = %+v", ps)
	}
	if err := c.DeleteCompanion(ctx, a.ID, spool.ID, RoleProxy, "mp4"); err != nil {
		t.Fatal(err)
	}
	if ps, _ = c.Companions(ctx, a.ID); len(ps) != 2 {
		t.Errorf("companions after delete = %+v", ps)
	}
	if RoleProxy.Regenerable() == false || RoleSidecar.Regenerable() {
		t.Error("Regenerable wrong")
	}

	id, err := c.BeginImport(ctx, spool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.FinishImport(ctx, id, 3, 4, 1); err != nil {
		t.Fatal(err)
	}
	im, err := c.Import(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if im.New != 3 || im.Skipped != 4 || im.Failed != 1 || im.FinishedAt.IsZero() || im.StartedAt.IsZero() {
		t.Errorf("import = %+v", im)
	}
}
