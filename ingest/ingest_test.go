package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scottlaird/mediamanager/catalog"
	"github.com/scottlaird/mediamanager/config"
	"github.com/scottlaird/mediamanager/identity"
	"github.com/scottlaird/mediamanager/linktree"
	"github.com/scottlaird/mediamanager/media"
	"github.com/scottlaird/mediamanager/naming"
)

const mib = 1 << 20

var (
	ctx  = context.Background()
	shot = time.Date(2026, 9, 5, 14, 0, 0, 0, time.UTC)
)

type fixture struct {
	base, card, spool, spool2, nas, links string
	env                                   *Env
}

// audioShared configures audio to live in the video tree, on disk and in
// the link tree; audioOwn gives it a tree of its own.
const (
	audioNone   = ""
	audioOwn    = "  audio: {link: %[1]s/audio}\n"
	audioShared = "  audio: {subdir: video, link: %[1]s/video}\n"
)

// newFixture builds spool, nas and link roots under a temp dir and an Env
// with absolute-path locations. audio is one of the audio* templates.
// Sources are made separately with mkCard.
func newFixture(t *testing.T, audio string) *fixture {
	t.Helper()
	f := &fixture{base: t.TempDir()}
	f.spool = filepath.Join(f.base, "spool")
	f.spool2 = filepath.Join(f.base, "spool2")
	f.nas = filepath.Join(f.base, "nas")
	f.links = filepath.Join(f.base, "links")
	for _, d := range []string{f.spool, f.spool2, f.nas} {
		os.MkdirAll(d, 0o755)
	}
	if audio != "" {
		audio = fmt.Sprintf(audio, f.links)
	}
	yaml := fmt.Sprintf(`
catalog: %s/catalog.db
timezone: UTC
trees:
  video: {link: %s/video}
  still: {subdir: stills, link: %s/still}
%s
locations:
  - {name: nas, kind: nas, path: %s}
  - {name: fast, kind: spool, path: %s, priority: 1}
  - {name: slow, kind: spool, path: %s, priority: 2}
`, f.base, f.links, f.links, audio, f.nas, f.spool, f.spool2)
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(cfg.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	f.env = &Env{Config: cfg, Catalog: cat, Logf: t.Logf}
	return f
}

// mkCard writes files (rel → size) under a new source dir, with fixed
// mtimes so capture dates are predictable. Content is seeded by name so
// identical names in different cards mean identical content unless the
// seed is varied.
func mkCard(t *testing.T, base, name string, files map[string]int, seed int64) string {
	t.Helper()
	root := filepath.Join(base, name)
	for rel, n := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		b := make([]byte, n)
		h := int64(0)
		for _, c := range rel {
			h = h*31 + int64(c)
		}
		rand.New(rand.NewSource(h ^ seed)).Read(b)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, shot, shot)
	}
	return root
}

func (f *fixture) relOf(t *testing.T, cardPath string, kind media.Kind) string {
	t.Helper()
	na := naming.Asset{Kind: kind, OrigName: filepath.Base(cardPath), CaptureTime: shot}
	if kind.UsesSparseID() {
		id, _, err := identity.SparseFile(cardPath)
		if err != nil {
			t.Fatal(err)
		}
		na.ID = id
	}
	rel, err := naming.DateScheme{}.Path(na)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

func readlink(t *testing.T, p string) string {
	t.Helper()
	s, err := os.Readlink(p)
	if err != nil {
		t.Fatalf("readlink %s: %v", p, err)
	}
	return s
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func sameContent(t *testing.T, a, b string) bool {
	t.Helper()
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(x, y)
}

func TestImportLifecycle(t *testing.T) {
	f := newFixture(t, audioNone)
	card := mkCard(t, f.base, "card", map[string]int{
		"A001_C001.braw":      3 * mib,
		"A001_C002.braw":      2*mib + 500,
		"Proxy/A001_C001.mp4": 64 * 1024,
		"Proxy/A001_C002.mp4": 64 * 1024,
		".DS_Store":           10,
		"notes.txt":           10,
		"DCIM-less-still.DNG": 100 * 1024,
	}, 1)
	c1 := f.relOf(t, filepath.Join(card, "A001_C001.braw"), media.Video)
	c2 := f.relOf(t, filepath.Join(card, "A001_C002.braw"), media.Video)
	still := "2026/09/05/dcim-less-still.dng"

	sum, err := f.env.Import(ctx, card)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Failures) != 0 {
		t.Fatalf("failures: %+v", sum.Failures)
	}
	if len(sum.Source.Assets) != 3 || len(sum.Source.New) != 3 || sum.Spooled != 3 || sum.Archived != 3 || !sum.SafeToFormat {
		t.Errorf("summary: assets=%d new=%d spooled=%d archived=%d safe=%v", len(sum.Source.Assets), len(sum.Source.New), sum.Spooled, sum.Archived, sum.SafeToFormat)
	}
	if !equalStrings(sum.Source.Unrecognised, []string{"notes.txt"}) {
		t.Errorf("unrecognised = %v", sum.Source.Unrecognised)
	}

	// Files where they should be, on both tiers, byte-identical.
	for _, rel := range []string{"video/" + c1, "video/" + c2, "stills/" + still, "video/" + naming.ProxyPath(c1, "mp4")} {
		for _, root := range []string{f.spool, f.nas} {
			if !exists(filepath.Join(root, rel)) {
				t.Errorf("%s missing from %s", rel, root)
			}
		}
	}
	if !sameContent(t, filepath.Join(card, "A001_C001.braw"), filepath.Join(f.nas, "video", c1)) {
		t.Error("NAS copy differs from card")
	}
	// Links point at the fast spool.
	if got := readlink(t, filepath.Join(f.links, "video", c1)); got != filepath.Join(f.spool, "video", c1) {
		t.Errorf("link -> %s", got)
	}
	if got := readlink(t, filepath.Join(f.links, "video", naming.ProxyPath(c2, "mp4"))); got != filepath.Join(f.spool, "video", naming.ProxyPath(c2, "mp4")) {
		t.Errorf("proxy link -> %s", got)
	}
	if got := readlink(t, filepath.Join(f.links, "still", still)); got != filepath.Join(f.spool, "stills", still) {
		t.Errorf("still link -> %s", got)
	}
	if bad, err := linktree.Audit(filepath.Join(f.links, "video")); err != nil {
		t.Errorf("audit: %v %v", bad, err)
	}
	// Nothing spooled to the slow spool.
	if entries, _ := os.ReadDir(f.spool2); len(entries) != 0 {
		t.Errorf("slow spool used: %v", entries)
	}

	// Second import: everything known, nothing copied, nothing rewritten.
	before, _ := os.Stat(filepath.Join(f.nas, "video", c1))
	sum, err = f.env.Import(ctx, card)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Source.New) != 0 || sum.Spooled != 0 || sum.Archived != 0 || !sum.SafeToFormat || len(sum.Failures) != 0 {
		t.Errorf("second import: new=%d spooled=%d archived=%d safe=%v fail=%v", len(sum.Source.New), sum.Spooled, sum.Archived, sum.SafeToFormat, sum.Failures)
	}
	after, _ := os.Stat(filepath.Join(f.nas, "video", c1))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("NAS file rewritten on re-import")
	}

	st, err := f.env.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, as := range st.Assets {
		if as.State != "archived" {
			t.Errorf("%s state %s, want archived (%v)", as.Asset.RelPath, as.State, as.Copies)
		}
	}

	// Flush: dry run first, then for real.
	dry, err := f.env.FlushSpool(ctx, "fast", FlushOptions{DryRun: true})
	if err != nil || len(dry.Flushed) != 3 || len(dry.Refused) != 0 {
		t.Fatalf("dry run: %+v, %v", dry, err)
	}
	if !exists(filepath.Join(f.spool, "video", c1)) {
		t.Fatal("dry run deleted")
	}
	rep, err := f.env.FlushSpool(ctx, "fast", FlushOptions{})
	if err != nil || len(rep.Flushed) != 3 || len(rep.Refused) != 0 {
		t.Fatalf("flush: %+v, %v", rep, err)
	}
	for _, rel := range []string{"video/" + c1, "video/" + naming.ProxyPath(c1, "mp4"), "stills/" + still} {
		if exists(filepath.Join(f.spool, rel)) {
			t.Errorf("%s still in spool after flush", rel)
		}
		if !exists(filepath.Join(f.nas, rel)) {
			t.Errorf("%s gone from NAS after flush", rel)
		}
	}
	if got := readlink(t, filepath.Join(f.links, "video", c1)); got != filepath.Join(f.nas, "video", c1) {
		t.Errorf("after flush link -> %s", got)
	}
	if got := readlink(t, filepath.Join(f.links, "video", naming.ProxyPath(c1, "mp4"))); got != filepath.Join(f.nas, "video", naming.ProxyPath(c1, "mp4")) {
		t.Errorf("after flush proxy link -> %s", got)
	}
	st, _ = f.env.Status(ctx)
	for _, as := range st.Assets {
		if as.State != "flushed" {
			t.Errorf("%s state %s after flush", as.Asset.RelPath, as.State)
		}
	}

	// Card pulled: links unaffected, re-import of the same card later is a no-op.
	os.RemoveAll(card)
	if _, err := f.env.Relink(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readlink(t, filepath.Join(f.links, "video", c1)); got != filepath.Join(f.nas, "video", c1) {
		t.Errorf("after unplug link -> %s", got)
	}
	card = mkCard(t, f.base, "card", map[string]int{"A001_C001.braw": 3 * mib}, 1)
	sum, err = f.env.Import(ctx, card)
	if err != nil || sum.Spooled != 0 || sum.Archived != 0 || !sum.SafeToFormat {
		t.Errorf("re-import after flush: %+v, %v", sum, err)
	}
}

func TestImportStillNameClashAndDuplicateContent(t *testing.T) {
	f := newFixture(t, audioNone)
	card := mkCard(t, f.base, "card", map[string]int{
		"DCIM/100_PANA/P1000001.JPG": 50 * 1024, // content A
		"DCIM/101_PANA/P1000001.JPG": 60 * 1024, // same name, content B
		"DCIM/102_PANA/P1000001.JPG": 50 * 1024, // overwritten below with A
		"DCIM/102_PANA/ZDUP.JPG":     50 * 1024, // overwritten below with A
	}, 7)
	src, _ := os.ReadFile(filepath.Join(card, "DCIM/100_PANA/P1000001.JPG"))
	for _, dup := range []string{"DCIM/102_PANA/P1000001.JPG", "DCIM/102_PANA/ZDUP.JPG"} {
		p := filepath.Join(card, dup)
		os.WriteFile(p, src, 0o644)
		os.Chtimes(p, shot, shot)
	}
	sum, err := f.env.Import(ctx, card)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Failures) != 0 {
		t.Fatalf("failures: %+v", sum.Failures)
	}
	// Four files, two distinct contents: two assets, the clash gets _1,
	// duplicates are recorded as extra source copies of the first asset.
	if len(sum.Source.Assets) != 4 || len(sum.Source.New) != 2 || sum.Spooled != 2 || sum.Archived != 2 {
		t.Errorf("assets=%d new=%d spooled=%d archived=%d", len(sum.Source.Assets), len(sum.Source.New), sum.Spooled, sum.Archived)
	}
	for _, rel := range []string{"stills/2026/09/05/p1000001.jpg", "stills/2026/09/05/p1000001_1.jpg"} {
		if !exists(filepath.Join(f.nas, rel)) {
			t.Errorf("%s missing from NAS", rel)
		}
	}
	if exists(filepath.Join(f.nas, "stills/2026/09/05/zdup.jpg")) {
		t.Error("duplicate content got its own file")
	}
	entries, _ := os.ReadDir(filepath.Join(f.nas, "stills/2026/09/05"))
	if len(entries) != 2 {
		t.Errorf("NAS day dir has %d entries, want 2", len(entries))
	}
	if !sum.SafeToFormat {
		t.Error("not safe to format after full import")
	}
}

func TestSpoolResumesPartial(t *testing.T) {
	f := newFixture(t, audioNone)
	card := mkCard(t, f.base, "card", map[string]int{"A001_C001.braw": 4 * mib}, 3)
	rel := f.relOf(t, filepath.Join(card, "A001_C001.braw"), media.Video)
	src, _ := os.ReadFile(filepath.Join(card, "A001_C001.braw"))
	partial := filepath.Join(f.spool, "video", rel+".partial")
	os.MkdirAll(filepath.Dir(partial), 0o755)
	os.WriteFile(partial, src[:3*mib], 0o644)

	ps, err := f.env.places(ctx)
	if err != nil {
		t.Fatal(err)
	}
	srcInfo, err := f.env.ScanSource(ctx, card)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.env.Spool(ctx, ps, srcInfo.Assets[0])
	if err != nil {
		t.Fatal(err)
	}
	if r.Resumed != 3*mib || r.Bytes != 4*mib || r.Location != "fast" || r.Copied != mib || r.Duration <= 0 || r.MiBPerSecond <= 0 {
		t.Errorf("result %+v", r)
	}
	if !sameContent(t, filepath.Join(card, "A001_C001.braw"), filepath.Join(f.spool, "video", rel)) {
		t.Error("resumed copy differs")
	}
	if got := readlink(t, filepath.Join(f.links, "video", rel)); got != filepath.Join(f.spool, "video", rel) {
		t.Errorf("link -> %s", got)
	}
	// Spooling again is a no-op.
	if r, err := f.env.Spool(ctx, ps, srcInfo.Assets[0]); err != nil || !r.Skipped {
		t.Errorf("second spool: %+v, %v", r, err)
	}
}

func TestSpoolFullArchivesFromSource(t *testing.T) {
	f := newFixture(t, audioNone)
	old := freeSpace
	freeSpace = func(string) (int64, error) { return 0, nil }
	t.Cleanup(func() { freeSpace = old })

	card := mkCard(t, f.base, "card", map[string]int{"A001_C001.braw": 2 * mib}, 5)
	rel := f.relOf(t, filepath.Join(card, "A001_C001.braw"), media.Video)
	sum, err := f.env.Import(ctx, card)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.SpoolFull) != 1 || sum.Spooled != 0 || sum.Archived != 1 || len(sum.Failures) != 0 || !sum.SafeToFormat {
		t.Errorf("summary: %+v", sum)
	}
	if exists(filepath.Join(f.spool, "video", rel)) {
		t.Error("spooled despite no room")
	}
	if got := readlink(t, filepath.Join(f.links, "video", rel)); got != filepath.Join(f.nas, "video", rel) {
		t.Errorf("link -> %s, want NAS", got)
	}
}

func TestUnmountedLocations(t *testing.T) {
	f := newFixture(t, audioNone)
	os.RemoveAll(f.spool) // fast spool "not mounted"
	os.RemoveAll(f.nas)   // NAS not mounted either
	card := mkCard(t, f.base, "card", map[string]int{"A001_C001.braw": 2 * mib}, 9)
	rel := f.relOf(t, filepath.Join(card, "A001_C001.braw"), media.Video)

	sum, err := f.env.Import(ctx, card)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Spooled != 1 || sum.Archived != 0 || sum.SafeToFormat || len(sum.Failures) != 0 {
		t.Errorf("summary: spooled=%d archived=%d safe=%v fail=%v", sum.Spooled, sum.Archived, sum.SafeToFormat, sum.Failures)
	}
	if !exists(filepath.Join(f.spool2, "video", rel)) {
		t.Error("slow spool not used when fast is absent")
	}
	if got := readlink(t, filepath.Join(f.links, "video", rel)); got != filepath.Join(f.spool2, "video", rel) {
		t.Errorf("link -> %s", got)
	}
	st, _ := f.env.Status(ctx)
	if st.Locations[0].Mounted || !st.Locations[2].Mounted {
		t.Errorf("locations: %+v", st.Locations)
	}
	// Flushing an unmounted spool, or with the NAS away, must refuse.
	if _, err := f.env.FlushSpool(ctx, "fast", FlushOptions{}); err == nil {
		t.Error("flushed an unmounted spool")
	}
	rep, err := f.env.FlushSpool(ctx, "slow", FlushOptions{})
	if err != nil || len(rep.Flushed) != 0 {
		t.Errorf("flush without NAS: %+v, %v", rep, err)
	}
	if !exists(filepath.Join(f.spool2, "video", rel)) {
		t.Fatal("spool copy deleted with no NAS copy anywhere")
	}

	// NAS comes back: ArchiveAll drains the backlog, then flush works.
	os.MkdirAll(f.nas, 0o755)
	as, err := f.env.ArchiveAll(ctx)
	if err != nil || as.Archived != 1 {
		t.Fatalf("ArchiveAll: %+v, %v", as, err)
	}
	rep, err = f.env.FlushSpool(ctx, "slow", FlushOptions{})
	if err != nil || len(rep.Flushed) != 1 {
		t.Fatalf("flush after archive: %+v, %v", rep, err)
	}
	if got := readlink(t, filepath.Join(f.links, "video", rel)); got != filepath.Join(f.nas, "video", rel) {
		t.Errorf("link -> %s", got)
	}
}

func TestFlushRefusesCorruptNAS(t *testing.T) {
	f := newFixture(t, audioNone)
	card := mkCard(t, f.base, "card", map[string]int{"A001_C001.braw": 3 * mib}, 11)
	rel := f.relOf(t, filepath.Join(card, "A001_C001.braw"), media.Video)
	if _, err := f.env.Import(ctx, card); err != nil {
		t.Fatal(err)
	}
	nasFile := filepath.Join(f.nas, "video", rel)
	b, _ := os.ReadFile(nasFile)
	os.WriteFile(nasFile, b[:len(b)-1], 0o644) // truncated after the fact

	rep, err := f.env.FlushSpool(ctx, "fast", FlushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Flushed) != 0 || len(rep.Refused) != 1 {
		t.Errorf("flush: %+v", rep)
	}
	for _, reason := range rep.Refused {
		if !strings.Contains(reason, "bytes") {
			t.Errorf("reason %q", reason)
		}
	}
	if !exists(filepath.Join(f.spool, "video", rel)) {
		t.Fatal("spool copy deleted despite bad NAS copy")
	}
}

func TestImportStopsOnRealFileInLinkTree(t *testing.T) {
	f := newFixture(t, audioNone)
	os.MkdirAll(filepath.Join(f.links, "video"), 0o755)
	os.WriteFile(filepath.Join(f.links, "video", "oops.braw"), []byte("real"), 0o644)
	card := mkCard(t, f.base, "card", map[string]int{"A001_C001.braw": mib}, 13)
	_, err := f.env.Import(ctx, card)
	if !errors.Is(err, linktree.ErrRealFiles) {
		t.Fatalf("err = %v, want ErrRealFiles", err)
	}
	if entries, _ := os.ReadDir(f.spool); len(entries) != 0 {
		t.Error("copied despite failed audit")
	}
}

func TestUnroutedAndOrphanProxies(t *testing.T) {
	f := newFixture(t, audioNone) // no audio tree
	card := mkCard(t, f.base, "card", map[string]int{
		"ZOOM0001.WAV":        mib,
		"Proxy/LONELY.mp4":    1024,
		"A001_C001.braw":      mib,
		"Proxy/A001_C001.mp4": 1024,
	}, 17)
	sum, err := f.env.Import(ctx, card)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(sum.Source.Unrouted, []string{"ZOOM0001.WAV"}) {
		t.Errorf("unrouted = %v", sum.Source.Unrouted)
	}
	if !equalStrings(sum.Source.Orphans, []string{"Proxy/LONELY.mp4"}) {
		t.Errorf("orphans = %v", sum.Source.Orphans)
	}
	if len(sum.Source.Assets) != 1 || !sum.SafeToFormat {
		t.Errorf("summary: %+v", sum)
	}
}

func TestAudioSharesVideoTree(t *testing.T) {
	f := newFixture(t, audioShared)
	card := mkCard(t, f.base, "card", map[string]int{
		"A001_C001.braw": 2 * mib,
		"ZOOM0001.WAV":   mib,
	}, 21)
	clip := f.relOf(t, filepath.Join(card, "A001_C001.braw"), media.Video)
	wav := f.relOf(t, filepath.Join(card, "ZOOM0001.WAV"), media.Audio)

	sum, err := f.env.Import(ctx, card)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Failures) != 0 || len(sum.Source.Unrouted) != 0 || sum.Spooled != 2 || sum.Archived != 2 || !sum.SafeToFormat {
		t.Fatalf("summary: %+v", sum)
	}
	// Both land in the same day directory on every tier and in one link tree.
	for _, rel := range []string{clip, wav} {
		for _, root := range []string{f.spool, f.nas} {
			if !exists(filepath.Join(root, "video", rel)) {
				t.Errorf("%s missing from %s/video", rel, root)
			}
		}
		if got := readlink(t, filepath.Join(f.links, "video", rel)); got != filepath.Join(f.spool, "video", rel) {
			t.Errorf("%s -> %s", rel, got)
		}
	}
	if filepath.Dir(clip) != filepath.Dir(wav) {
		t.Errorf("clip and wav in different directories: %s vs %s", clip, wav)
	}

	// One reconcile pass for the shared root, and it sees no strays.
	rep, err := f.env.Relink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Trees) != 2 { // video+audio share one root; still has its own
		t.Errorf("reconciled %d roots, want 2: %v", len(rep.Trees), rep.Trees)
	}
	shared := rep.Trees[filepath.Join(f.links, "video")]
	if len(shared.Unknown) != 0 || shared.Unchanged != 2 || len(shared.Updated)+len(shared.Created) != 0 {
		t.Errorf("shared tree report: %+v", shared)
	}

	// Flushing moves both links to the NAS; nothing is orphaned or pruned.
	if _, err := f.env.FlushSpool(ctx, "fast", FlushOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{clip, wav} {
		if got := readlink(t, filepath.Join(f.links, "video", rel)); got != filepath.Join(f.nas, "video", rel) {
			t.Errorf("after flush %s -> %s", rel, got)
		}
	}
	if bad, err := linktree.Audit(filepath.Join(f.links, "video")); err != nil {
		t.Errorf("audit: %v %v", bad, err)
	}
}

func TestAudioOwnTreeStaysSeparate(t *testing.T) {
	f := newFixture(t, audioOwn)
	card := mkCard(t, f.base, "card", map[string]int{
		"A001_C001.braw": mib,
		"ZOOM0001.WAV":   mib,
	}, 23)
	if _, err := f.env.Import(ctx, card); err != nil {
		t.Fatal(err)
	}
	rep, err := f.env.Relink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Trees) != 3 {
		t.Errorf("reconciled %d roots, want 3", len(rep.Trees))
	}
	for root, r := range rep.Trees {
		if len(r.Unknown) != 0 {
			t.Errorf("%s: unknown links %v", root, r.Unknown)
		}
	}
	if !exists(filepath.Join(f.nas, "audio", "2026/09/05")) || !exists(filepath.Join(f.links, "audio", "2026/09/05")) {
		t.Error("audio not in its own tree")
	}
}

func TestSidecarsTravelAndStayCurrent(t *testing.T) {
	f := newFixture(t, audioNone)
	card := mkCard(t, f.base, "card", map[string]int{
		"A005_12062359_C001.braw":          2 * mib,
		"A005_12062359_C001.sidecar":       900,
		"Proxy/A005_12062359_C001.mp4":     4096,
		"Proxy/A005_12062359_C001.sidecar": 800,
	}, 31)
	clip := f.relOf(t, filepath.Join(card, "A005_12062359_C001.braw"), media.Video)
	side := naming.SidecarPath(clip, "sidecar")
	pside := naming.ProxySidecarPath(clip, "sidecar")

	sum, err := f.env.Import(ctx, card)
	if err != nil || len(sum.Failures) != 0 || len(sum.Source.Orphans) != 0 {
		t.Fatalf("import: %+v, %v", sum, err)
	}
	for _, rel := range []string{side, pside, naming.ProxyPath(clip, "mp4")} {
		for _, root := range []string{f.spool, f.nas} {
			if !exists(filepath.Join(root, "video", rel)) {
				t.Errorf("%s missing from %s", rel, root)
			}
		}
		if got := readlink(t, filepath.Join(f.links, "video", rel)); got != filepath.Join(f.spool, "video", rel) {
			t.Errorf("%s -> %s", rel, got)
		}
	}
	if !sameContent(t, filepath.Join(card, "A005_12062359_C001.sidecar"), filepath.Join(f.nas, "video", side)) {
		t.Error("NAS sidecar differs from card")
	}

	// The editor writes to the sidecar through the link (i.e. the spool copy).
	edited := []byte("edited by resolve: new colour science")
	if err := os.WriteFile(filepath.Join(f.links, "video", side), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if sameContent(t, filepath.Join(f.spool, "video", side), filepath.Join(f.nas, "video", side)) {
		t.Fatal("edit through the link did not land in the spool")
	}
	// A later import of the same card refreshes the NAS sidecar without touching the original.
	before, _ := os.Stat(filepath.Join(f.nas, "video", clip))
	if _, err := f.env.Import(ctx, card); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(f.nas, "video", clip))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("original rewritten while syncing sidecars")
	}
	if got, _ := os.ReadFile(filepath.Join(f.nas, "video", side)); string(got) != string(edited) {
		t.Errorf("NAS sidecar not refreshed: %q", got)
	}

	// Edit again, then flush: the sidecar is synced before it leaves the spool.
	edited2 := []byte("edited again just before flush")
	os.WriteFile(filepath.Join(f.links, "video", side), edited2, 0o644)
	rep, err := f.env.FlushSpool(ctx, "fast", FlushOptions{})
	if err != nil || len(rep.Flushed) != 1 {
		t.Fatalf("flush: %+v, %v", rep, err)
	}
	for _, rel := range []string{clip, side, pside, naming.ProxyPath(clip, "mp4")} {
		if exists(filepath.Join(f.spool, "video", rel)) {
			t.Errorf("%s still in spool", rel)
		}
		if got := readlink(t, filepath.Join(f.links, "video", rel)); got != filepath.Join(f.nas, "video", rel) {
			t.Errorf("after flush %s -> %s", rel, got)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(f.nas, "video", side)); string(got) != string(edited2) {
		t.Errorf("NAS sidecar after flush: %q", got)
	}
	if bad, err := linktree.Audit(filepath.Join(f.links, "video")); err != nil {
		t.Errorf("audit: %v %v", bad, err)
	}
}

func TestEditorCreatedSidecarsAreSwept(t *testing.T) {
	f := newFixture(t, audioNone)
	card := mkCard(t, f.base, "card", map[string]int{
		"A001_C001.braw":             2 * mib,
		"DCIM/100_PANA/P1000123.RW2": 300 * 1024,
		"DCIM/100_PANA/P1000123.JPG": 100 * 1024,
	}, 41)
	// A DCIM card whose root also has a flat clip: make it flat by moving DCIM contents up.
	os.Rename(filepath.Join(card, "DCIM/100_PANA/P1000123.RW2"), filepath.Join(card, "P1000123.RW2"))
	os.Rename(filepath.Join(card, "DCIM/100_PANA/P1000123.JPG"), filepath.Join(card, "P1000123.JPG"))
	os.RemoveAll(filepath.Join(card, "DCIM"))
	clip := f.relOf(t, filepath.Join(card, "A001_C001.braw"), media.Video)
	if _, err := f.env.Import(ctx, card); err != nil {
		t.Fatal(err)
	}

	// Resolve creates a .sidecar beside the clip link; Lightroom writes an
	// .xmp beside the raw's link (the JPEG shares the base name).
	sideLink := filepath.Join(f.links, "video", naming.SidecarPath(clip, "sidecar"))
	xmpLink := filepath.Join(f.links, "still", "2026/09/05/p1000123.xmp")
	os.WriteFile(sideLink, []byte("braw colour settings"), 0o644)
	os.WriteFile(xmpLink, []byte("<x:xmpmeta/>"), 0o644)
	// A real media file in the tree is still a hard stop.
	stray := filepath.Join(f.links, "video", "2026/09/05/dropped.braw")
	os.WriteFile(stray, []byte("not a link"), 0o644)
	if _, err := f.env.Relink(ctx); !errors.Is(err, linktree.ErrRealFiles) {
		t.Fatalf("relink with a real media file: %v", err)
	}
	os.Remove(stray)

	rep, err := f.env.Relink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unavailable) != 0 {
		t.Errorf("unavailable: %v", rep.Unavailable)
	}
	// Both sidecars now live in the spool beside their originals and are linked.
	spoolSide := filepath.Join(f.spool, "video", naming.SidecarPath(clip, "sidecar"))
	spoolXMP := filepath.Join(f.spool, "stills", "2026/09/05/p1000123.xmp")
	for link, target := range map[string]string{sideLink: spoolSide, xmpLink: spoolXMP} {
		if !exists(target) {
			t.Errorf("%s not swept into spool", target)
		}
		if got := readlink(t, link); got != target {
			t.Errorf("%s -> %s, want %s", link, got, target)
		}
	}
	if b, _ := os.ReadFile(spoolXMP); string(b) != "<x:xmpmeta/>" {
		t.Errorf("xmp content %q", b)
	}
	// The xmp belongs to the raw, not the JPEG.
	raw, err := f.env.Catalog.AssetByPath(ctx, media.Still, "2026/09/05/p1000123.rw2")
	if err != nil {
		t.Fatal(err)
	}
	comps, _ := f.env.Catalog.Companions(ctx, raw.ID)
	if len(comps) != 1 || comps[0].Role != catalog.RoleSidecar || comps[0].Ext != "xmp" {
		t.Errorf("raw companions = %+v", comps)
	}
	jpg, _ := f.env.Catalog.AssetByPath(ctx, media.Still, "2026/09/05/p1000123.jpg")
	if c, _ := f.env.Catalog.Companions(ctx, jpg.ID); len(c) != 0 {
		t.Errorf("jpeg got the xmp: %+v", c)
	}
	// Flush carries them to the NAS and repoints the links.
	if _, err := f.env.FlushSpool(ctx, "fast", FlushOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := readlink(t, xmpLink); got != filepath.Join(f.nas, "stills", "2026/09/05/p1000123.xmp") {
		t.Errorf("xmp after flush -> %s", got)
	}
	if got := readlink(t, sideLink); got != filepath.Join(f.nas, "video", naming.SidecarPath(clip, "sidecar")) {
		t.Errorf("sidecar after flush -> %s", got)
	}
	for _, root := range []string{filepath.Join(f.links, "video"), filepath.Join(f.links, "still")} {
		if bad, err := linktree.Audit(root); err != nil {
			t.Errorf("audit %s: %v %v", root, bad, err)
		}
	}
}

func TestTreeAtLocationRoot(t *testing.T) {
	f := newFixture(t, audioNone)
	tr := f.env.Config.Trees["video"]
	tr.Subdir = "."
	f.env.Config.Trees["video"] = tr
	// An existing spool holding clips at its root, like /Volumes/m2/2026/...
	old := filepath.Join(f.spool, "2026", "A021_07100435_C001.braw")
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.WriteFile(old, bytes.Repeat([]byte("m2"), mib), 0o644)
	os.Chtimes(old, shot, shot)
	rel := f.relOf(t, old, media.Video)

	rep, err := f.env.Adopt(ctx, "fast", AdoptOptions{Mode: Migrate})
	if err != nil || rep.Counts["rename"] != 1 {
		t.Fatalf("adopt: %v, %v", rep.Counts, err)
	}
	if !exists(filepath.Join(f.spool, rel)) || exists(old) {
		t.Fatalf("migrate at root: %s not at %s", old, rel)
	}
	if got := readlink(t, filepath.Join(f.links, "video", rel)); got != filepath.Join(f.spool, rel) {
		t.Errorf("link -> %s", got)
	}
	// Archive lands it at the NAS root under the same relpath; flush then works.
	sum, err := f.env.ArchiveAll(ctx)
	if err != nil || sum.Archived != 1 || !exists(filepath.Join(f.nas, rel)) {
		t.Fatalf("archive: %+v, %v", sum, err)
	}
	fr, err := f.env.FlushSpool(ctx, "fast", FlushOptions{})
	if err != nil || len(fr.Flushed) != 1 {
		t.Fatalf("flush: %+v, %v", fr, err)
	}
	if got := readlink(t, filepath.Join(f.links, "video", rel)); got != filepath.Join(f.nas, rel) {
		t.Errorf("after flush link -> %s", got)
	}
	// The stills tree keeps its own subdir alongside.
	card := mkCard(t, f.base, "card", map[string]int{"L1.DNG": 4096}, 3)
	if _, err := f.env.Import(ctx, card); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(f.nas, "stills", "2026/09/05/l1.dng")) {
		t.Error("stills tree lost its subdir")
	}
}

func TestList(t *testing.T) {
	f := newFixture(t, audioNone)
	card := mkCard(t, f.base, "card", map[string]int{
		"A001_C001.braw":    2 * mib,
		"A001_C001.sidecar": 300,
		"B002_C001.braw":    mib,
		"L1004821.DNG":      50 * 1024,
	}, 51)
	if _, err := f.env.Import(ctx, card); err != nil {
		t.Fatal(err)
	}
	paths := func(as []AssetStatus) []string {
		var out []string
		for _, a := range as {
			out = append(out, a.Asset.Kind.String()+"/"+a.Asset.RelPath)
		}
		return out
	}
	all, err := f.env.List(ctx, ListOptions{Companions: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Asset.RelPath > all[1].Asset.RelPath {
		t.Fatalf("all = %v", paths(all))
	}
	var withSidecar int
	for _, a := range all {
		if len(a.Companions) > 0 && a.Companions[0].Role == catalog.RoleSidecar {
			withSidecar++ // one per location the sidecar reached: source, spool, NAS
			if len(a.Companions) != 3 {
				t.Errorf("%s: %d companion rows, want 3", a.Asset.RelPath, len(a.Companions))
			}
		}
		if len(a.CopyRows) < 3 { // source, spool, nas
			t.Errorf("%s: copy rows %d", a.Asset.RelPath, len(a.CopyRows))
		}
	}
	if withSidecar != 1 {
		t.Errorf("assets with a sidecar = %d", withSidecar)
	}
	tests := []struct {
		name string
		opts ListOptions
		want int
	}{
		{"kind still", ListOptions{Kind: media.Still}, 1},
		{"prefix relpath", ListOptions{Prefixes: []string{"2026/09/05/a001"}}, 1},
		{"prefix kind/relpath", ListOptions{Prefixes: []string{"video/2026"}}, 2},
		{"prefix day dir with slash", ListOptions{Prefixes: []string{"2026/09/05/"}}, 3},
		{"prefix miss", ListOptions{Prefixes: []string{"2025"}}, 0},
		{"two prefixes", ListOptions{Prefixes: []string{"still/", "2026/09/05/b002"}}, 2},
		{"state archived", ListOptions{States: []string{"archived"}}, 3},
		{"state flushed", ListOptions{States: []string{"flushed"}}, 0},
		{"location nas", ListOptions{Location: "nas"}, 3},
		{"location slow", ListOptions{Location: "slow"}, 0},
		{"combined", ListOptions{Kind: media.Video, Location: "fast", States: []string{"archived", "spooled"}}, 2},
	}
	for _, tt := range tests {
		got, err := f.env.List(ctx, tt.opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != tt.want {
			t.Errorf("%s: got %v, want %d", tt.name, paths(got), tt.want)
		}
	}
	if _, err := f.env.FlushSpool(ctx, "fast", FlushOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.env.List(ctx, ListOptions{States: []string{"flushed"}}); len(got) != 3 {
		t.Errorf("after flush: %v", paths(got))
	}
}

func TestArchivePlan(t *testing.T) {
	f := newFixture(t, audioNone)
	os.RemoveAll(f.nas) // NAS away during import: everything stays spooled
	card := mkCard(t, f.base, "card", map[string]int{"A001_C001.braw": 3 * mib, "B001_C001.braw": mib}, 61)
	if _, err := f.env.Import(ctx, card); err != nil {
		t.Fatal(err)
	}
	plan, err := f.env.ArchivePlan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 || plan[0].Reason != "no NAS is mounted" || plan[0].From == "" {
		t.Fatalf("plan without NAS: %+v", plan)
	}

	os.MkdirAll(f.nas, 0o755)
	// A partial from an interrupted earlier attempt, to be resumed.
	relA := f.relOf(t, filepath.Join(card, "A001_C001.braw"), media.Video)
	partial := filepath.Join(f.nas, "video", relA+".partial")
	os.MkdirAll(filepath.Dir(partial), 0o755)
	src, _ := os.ReadFile(filepath.Join(card, "A001_C001.braw"))
	os.WriteFile(partial, src[:mib], 0o644)

	plan, err = f.env.ArchivePlan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	for _, it := range plan {
		if it.Reason != "" || len(it.To) != 1 || it.To[0] != "nas" || !strings.HasPrefix(it.From, f.spool) {
			t.Errorf("item %+v", it)
		}
		if it.Asset.RelPath == relA && it.Resume != mib {
			t.Errorf("resume = %d, want %d", it.Resume, mib)
		}
	}
	// The plan changed nothing.
	if exists(filepath.Join(f.nas, "video", relA)) {
		t.Error("dry run archived")
	}
	if sum, err := f.env.ArchiveAll(ctx); err != nil || sum.Archived != 2 {
		t.Fatalf("archive: %+v, %v", sum, err)
	}
	if plan, _ = f.env.ArchivePlan(ctx); len(plan) != 0 {
		t.Errorf("plan after archive: %+v", plan)
	}
}

func TestSpoolBackAndSelect(t *testing.T) {
	f := newFixture(t, audioNone)
	card := mkCard(t, f.base, "card", map[string]int{"A001_C001.braw": 2 * mib, "B001_C001.braw": mib, "L1.DNG": 4096}, 71)
	if _, err := f.env.Import(ctx, card); err != nil {
		t.Fatal(err)
	}
	if _, err := f.env.FlushSpool(ctx, "fast", FlushOptions{}); err != nil {
		t.Fatal(err)
	}
	clip := f.relOf(t, filepath.Join(card, "A001_C001.braw"), media.Video)
	if got := readlink(t, filepath.Join(f.links, "video", clip)); got != filepath.Join(f.nas, "video", clip) {
		t.Fatalf("after flush link -> %s", got)
	}

	// Select by id, by relpath prefix, by kind/relpath prefix; reject junk.
	a, _ := f.env.Catalog.AssetByPath(ctx, media.Video, clip)
	for _, tt := range []struct {
		args []string
		want int
	}{
		{[]string{a.ID}, 1},
		{[]string{"2026/09/05/a001"}, 1},
		{[]string{"video/2026"}, 2},
		{[]string{"2026"}, 3},
		{[]string{a.ID, "2026/09/05/a001"}, 1}, // deduplicated
	} {
		refs, err := f.env.Select(ctx, tt.args)
		if err != nil || len(refs) != tt.want {
			t.Errorf("Select(%v) = %d refs, %v; want %d", tt.args, len(refs), err, tt.want)
		}
	}
	if _, err := f.env.Select(ctx, []string{"nope"}); err == nil {
		t.Error("unknown selector accepted")
	}

	refs, _ := f.env.Select(ctx, []string{"video/2026"})
	sum, err := f.env.SpoolAll(ctx, refs, true)
	if err != nil || sum.Spooled != 2 || sum.Skipped != 0 || len(sum.Failures) != 0 || sum.Copied != 3*mib {
		t.Fatalf("spool back: %+v, %v", sum, err)
	}
	if got := readlink(t, filepath.Join(f.links, "video", clip)); got != filepath.Join(f.spool, "video", clip) {
		t.Errorf("after spool link -> %s", got)
	}
	if !sameContent(t, filepath.Join(f.nas, "video", clip), filepath.Join(f.spool, "video", clip)) {
		t.Error("spooled copy differs from NAS")
	}
	a, _ = f.env.Catalog.Asset(ctx, a.ID)
	if !a.Pinned {
		t.Error("not pinned")
	}
	rep, _ := f.env.FlushSpool(ctx, "fast", FlushOptions{})
	if len(rep.Flushed) != 0 {
		t.Errorf("flush removed pinned assets: %+v", rep)
	}
	sum, _ = f.env.SpoolAll(ctx, refs, false)
	if sum.Skipped != 2 || sum.Spooled != 0 {
		t.Errorf("second spool: %+v", sum)
	}
}

func TestGeneratedProxyInLinkTreeIsSwept(t *testing.T) {
	f := newFixture(t, audioNone)
	card := mkCard(t, f.base, "card", map[string]int{"A001_C001.braw": 2 * mib}, 73)
	if _, err := f.env.Import(ctx, card); err != nil {
		t.Fatal(err)
	}
	clip := f.relOf(t, filepath.Join(card, "A001_C001.braw"), media.Video)
	proxyRel := naming.ProxyPath(clip, "mov")
	// Blackmagic Proxy Generator, watching ~/Video, writes Proxy/<base>.mov beside the link.
	dropped := filepath.Join(f.links, "video", proxyRel)
	os.MkdirAll(filepath.Dir(dropped), 0o755)
	os.WriteFile(dropped, bytes.Repeat([]byte("proxy"), 200000), 0o644)

	if _, err := f.env.Relink(ctx); err != nil {
		t.Fatalf("relink with a generated proxy: %v", err)
	}
	spoolProxy := filepath.Join(f.spool, "video", proxyRel)
	if !exists(spoolProxy) {
		t.Fatal("proxy not swept into the spool")
	}
	if got := readlink(t, dropped); got != spoolProxy {
		t.Errorf("proxy link -> %s", got)
	}
	a, _ := f.env.Catalog.AssetByPath(ctx, media.Video, clip)
	comps, _ := f.env.Catalog.Companions(ctx, a.ID)
	if len(comps) != 1 || comps[0].Role != catalog.RoleProxy || comps[0].Ext != "mov" {
		t.Errorf("companions = %+v", comps)
	}
	// It reaches the NAS on the next archive pass, and survives a flush.
	if _, err := f.env.ArchiveAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.env.Import(ctx, card); err != nil { // already-archived path syncs companions
		t.Fatal(err)
	}
	nasProxy := filepath.Join(f.nas, "video", proxyRel)
	if !exists(nasProxy) {
		t.Fatal("proxy not copied to the NAS")
	}
	if _, err := f.env.FlushSpool(ctx, "fast", FlushOptions{}); err != nil {
		t.Fatal(err)
	}
	if exists(spoolProxy) || !exists(nasProxy) {
		t.Error("flush did not move the proxy's link to the NAS copy")
	}
	if got := readlink(t, dropped); got != nasProxy {
		t.Errorf("after flush proxy link -> %s", got)
	}
	// A proxy generated while the clip is already flushed goes to the NAS directly.
	os.Remove(dropped)
	os.WriteFile(dropped, bytes.Repeat([]byte("proxy2"), 100000), 0o644)
	if _, err := f.env.Relink(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(nasProxy); len(b) != 600000 {
		t.Errorf("regenerated proxy not placed on the NAS: %d bytes", len(b))
	}
}

func equalStrings(a, b []string) bool {
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
