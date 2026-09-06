package ingest

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scottlaird/mediamanager/catalog"
	"github.com/scottlaird/mediamanager/media"
)

// legacyLibrary lays out a miniature of the real library under the NAS's
// Library subdir: legacy names, raw camera names, a month-only 2026 dir,
// proxies with sidecars, an accidental nested copy, a same-content pair,
// junk, an empty label folder and a couple of stray extensions.
func legacyLibrary(t *testing.T, f *fixture) map[string]string {
	t.Helper()
	lib := filepath.Join(f.nas, "Library")
	put := func(rel string, size int, mtime time.Time, seed int64) {
		p := filepath.Join(lib, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		b := make([]byte, size)
		rand.New(rand.NewSource(seed)).Read(b)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mtime, mtime)
	}
	jan := time.Date(2024, 1, 28, 9, 30, 0, 0, time.UTC)
	oct := time.Date(2024, 10, 13, 1, 30, 0, 0, time.UTC)
	mar := time.Date(2025, 3, 22, 1, 20, 0, 0, time.UTC)
	dec := time.Date(2025, 12, 7, 0, 5, 0, 0, time.UTC)
	feb := time.Date(2026, 2, 8, 7, 21, 0, 0, time.UTC)

	put("2024/01/28/A006_01281024_C008_2024-01-28-092459_5a6cae2958776918.braw", 3*mib, jan, 1)
	put("2024/10/13/241013_001.WAV", 200*1024, oct, 2)
	put("2024/10/13/241013_001_2024-10-13-012325_ec31f965579ba42b.WAV", 200*1024, oct, 2) // same content
	put("2024/08/24/RECRST_IDX0_2024-08-24-155019_2ed6e3ee3708ee0c.SUCC", 100, oct, 3)
	put("2025/03/22/A008_03220117_C001_2025-03-22-011701_6058ba6e4b183c21.braw", 2*mib+7, mar, 4)
	put("2025/03/22/Proxy/A008_03220117_C001_2025-03-22-011701_6058ba6e4b183c21.mov", 8192, mar, 5)
	put("2025/03/22/20250318-kirkland/Videos/Library/2025/03/22/A008_03220117_C001_2025-03-22-011701_6058ba6e4b183c21.braw", 2*mib+7, mar, 4)
	put("2025/03/22/20250318-kirkland/Videos/Library/2025/03/22/Proxy/A008_03220117_C001_2025-03-22-011701_6058ba6e4b183c21.mov", 8192, mar, 5)
	put("2025/10/11/Video Assist_0007_2025-10-11-103450_20aa447b6c17707c.braw-old", 100, mar, 6)
	put("2025/12/06/A005_12062359_C001.braw", 2*mib, dec, 7)
	put("2025/12/06/A005_12062359_C001.sidecar", 700, dec, 8)
	put("2025/12/06/Proxy/A005_12062359_C001.mp4", 4096, dec, 9)
	put("2025/12/06/Proxy/A005_12062359_C001.sidecar", 600, dec, 10)
	put("2025/12/06/Stills/A005_12070025_S001.braw", mib, dec, 11)
	put("2026/02/L1000598.MOV", mib, feb, 12)
	put("2026/02/P1012643.MOV", mib, feb, 13)
	put("2024/01/28/.DS_Store", 10, jan, 14)
	os.MkdirAll(filepath.Join(lib, "2025/01/29/20250129-leavenworth"), 0o755)
	os.WriteFile(filepath.Join(lib, "2025/01/29/20250129-leavenworth/.DS_Store"), []byte("x"), 0o644)

	ids := map[string]string{}
	for _, rel := range []string{
		"2024/01/28/A006_01281024_C008_2024-01-28-092459_5a6cae2958776918.braw",
		"2024/10/13/241013_001.WAV",
		"2025/03/22/A008_03220117_C001_2025-03-22-011701_6058ba6e4b183c21.braw",
		"2025/12/06/A005_12062359_C001.braw",
		"2025/12/06/Stills/A005_12070025_S001.braw",
		"2026/02/L1000598.MOV",
		"2026/02/P1012643.MOV",
	} {
		kind := media.Video
		if strings.HasSuffix(rel, ".WAV") {
			kind = media.Audio
		}
		na := f.relOf(t, filepath.Join(lib, rel), kind)
		ids[rel] = strings.TrimSuffix(na[strings.LastIndex(na, "-")+1:], filepath.Ext(na))
	}
	return ids
}

// libraryFixture is newFixture with the video and audio trees sharing the
// NAS's Library subdir, like the real one.
func libraryFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, audioShared)
	// Rewrite the trees so video and audio live in "Library".
	for _, k := range []string{"video", "audio"} {
		tr := f.env.Config.Trees[k]
		tr.Subdir = "Library"
		f.env.Config.Trees[k] = tr
	}
	return f
}

func ops(rep *AdoptReport, op string) []AdoptAction {
	var out []AdoptAction
	for _, a := range rep.Actions {
		if a.Op == op {
			out = append(out, a)
		}
	}
	return out
}

func TestAdoptInPlace(t *testing.T) {
	f := libraryFixture(t)
	ids := legacyLibrary(t, f)
	lib := filepath.Join(f.nas, "Library")

	dry, err := f.env.Adopt(ctx, "nas", AdoptOptions{Mode: InPlace, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Counts["adopt"] != 7 || dry.Counts["duplicate"] != 2 || dry.Counts["unrecognised"] != 2 ||
		dry.Counts["orphan"] != 1 || dry.Counts["companion"] != 4 || dry.Counts["empty-dir"] != 1 || dry.Counts["rename"] != 0 {
		t.Errorf("dry-run counts: %v", dry.Counts)
	}
	if _, err := f.env.Catalog.Asset(ctx, ids["2026/02/L1000598.MOV"]); err == nil {
		t.Fatal("dry run wrote to the catalog")
	}

	rep, err := f.env.Adopt(ctx, "nas", AdoptOptions{Mode: InPlace})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Counts["adopt"] != 7 {
		t.Errorf("counts: %v", rep.Counts)
	}
	// Names and paths untouched; catalog rows point at them.
	legacy := "2024/01/28/A006_01281024_C008_2024-01-28-092459_5a6cae2958776918.braw"
	if !exists(filepath.Join(lib, legacy)) {
		t.Fatal("in-place adoption moved a file")
	}
	a, err := f.env.Catalog.Asset(ctx, ids[legacy])
	if err != nil {
		t.Fatal(err)
	}
	if a.RelPath != legacy || a.OrigName != "A006_01281024_C008.braw" || a.Kind != media.Video {
		t.Errorf("asset = %+v", a)
	}
	want := time.Date(2024, 1, 28, 9, 24, 59, 0, time.UTC)
	if !a.CaptureTime.Equal(want) {
		t.Errorf("capture time %v, want %v (from the legacy name)", a.CaptureTime, want)
	}
	if got := readlink(t, filepath.Join(f.links, "video", legacy)); got != filepath.Join(lib, legacy) {
		t.Errorf("link -> %s", got)
	}
	// The wav lives in the shared tree and links from the same root.
	wav := "2024/10/13/241013_001.WAV"
	if got := readlink(t, filepath.Join(f.links, "video", wav)); got != filepath.Join(lib, wav) {
		t.Errorf("wav link -> %s", got)
	}
	// Companions recorded and linked where they already are.
	comps, _ := f.env.Catalog.Companions(ctx, ids["2025/12/06/A005_12062359_C001.braw"])
	if len(comps) != 3 {
		t.Errorf("companions = %+v", comps)
	}
	if got := readlink(t, filepath.Join(f.links, "video", "2025/12/06/Proxy/A005_12062359_C001.sidecar")); got != filepath.Join(lib, "2025/12/06/Proxy/A005_12062359_C001.sidecar") {
		t.Errorf("proxy sidecar link -> %s", got)
	}
	// Duplicates are named, not touched, not catalogued twice.
	dups := ops(rep, "duplicate")
	if len(dups) != 2 || !strings.Contains(dups[0].Note, "241013_001.WAV") {
		t.Errorf("duplicates = %+v", dups)
	}
	if !exists(filepath.Join(lib, "2024/10/13/241013_001_2024-10-13-012325_ec31f965579ba42b.WAV")) {
		t.Error("duplicate removed")
	}
	st, _ := f.env.Status(ctx)
	if len(st.Assets) != 7 {
		t.Errorf("%d assets, want 7", len(st.Assets))
	}
	for _, as := range st.Assets {
		if as.State != "flushed" { // NAS only
			t.Errorf("%s: %s", as.Asset.RelPath, as.State)
		}
	}

	// Running again is a no-op that reports everything as known.
	again, err := f.env.Adopt(ctx, "nas", AdoptOptions{Mode: InPlace})
	if err != nil || again.Counts["known"] != 7 || again.Counts["adopt"] != 0 {
		t.Errorf("second adopt: %v, %v", again.Counts, err)
	}
}

func TestAdoptMigrate(t *testing.T) {
	f := libraryFixture(t)
	ids := legacyLibrary(t, f)
	lib := filepath.Join(f.nas, "Library")

	dry, err := f.env.Adopt(ctx, "nas", AdoptOptions{Mode: Migrate, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	wantMoves := map[string]string{
		"Library/2024/01/28/A006_01281024_C008_2024-01-28-092459_5a6cae2958776918.braw": "Library/2024/01/28/a006_01281024_c008-" + ids["2024/01/28/A006_01281024_C008_2024-01-28-092459_5a6cae2958776918.braw"] + ".braw",
		"Library/2024/10/13/241013_001.WAV":                                             "Library/2024/10/13/241013_001-" + ids["2024/10/13/241013_001.WAV"] + ".wav",
		"Library/2025/12/06/Stills/A005_12070025_S001.braw":                             "Library/2025/12/06/a005_12070025_s001-" + ids["2025/12/06/Stills/A005_12070025_S001.braw"] + ".braw",
		"Library/2026/02/L1000598.MOV":                                                  "Library/2026/02/08/l1000598-" + ids["2026/02/L1000598.MOV"] + ".mov",
	}
	got := map[string]string{}
	for _, a := range ops(dry, "rename") {
		got[a.From] = a.To
	}
	for from, to := range wantMoves {
		if got[from] != to {
			t.Errorf("plan for %s: got %q, want %q", from, got[from], to)
		}
	}
	if dry.Counts["rename"] != 7 || dry.Counts["duplicate"] != 2 || dry.Counts["companion"] != 4 {
		t.Errorf("dry-run counts: %v", dry.Counts)
	}
	if exists(filepath.Join(f.nas, wantMoves["Library/2026/02/L1000598.MOV"])) {
		t.Fatal("dry run renamed a file")
	}

	rep, err := f.env.Adopt(ctx, "nas", AdoptOptions{Mode: Migrate})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Counts["rename"] != 7 {
		t.Errorf("counts: %v", rep.Counts)
	}
	for from, to := range wantMoves {
		if exists(filepath.Join(f.nas, from)) {
			t.Errorf("%s still present", from)
		}
		if !exists(filepath.Join(f.nas, to)) {
			t.Errorf("%s missing", to)
		}
	}
	// Companions moved beside their originals under the new names.
	base := "2025/12/06/a005_12062359_c001-" + ids["2025/12/06/A005_12062359_C001.braw"]
	for _, rel := range []string{base + ".braw", base + ".sidecar", "2025/12/06/Proxy/a005_12062359_c001-" + ids["2025/12/06/A005_12062359_C001.braw"] + ".mp4", "2025/12/06/Proxy/a005_12062359_c001-" + ids["2025/12/06/A005_12062359_C001.braw"] + ".sidecar"} {
		if !exists(filepath.Join(lib, rel)) {
			t.Errorf("%s missing after migrate", rel)
		}
		if got := readlink(t, filepath.Join(f.links, "video", rel)); got != filepath.Join(lib, rel) {
			t.Errorf("%s -> %s", rel, got)
		}
	}
	// Duplicates, strays and the label folder are exactly where they were.
	for _, rel := range []string{
		"2024/10/13/241013_001_2024-10-13-012325_ec31f965579ba42b.WAV",
		"2025/03/22/20250318-kirkland/Videos/Library/2025/03/22/A008_03220117_C001_2025-03-22-011701_6058ba6e4b183c21.braw",
		"2024/08/24/RECRST_IDX0_2024-08-24-155019_2ed6e3ee3708ee0c.SUCC",
		"2025/10/11/Video Assist_0007_2025-10-11-103450_20aa447b6c17707c.braw-old",
		"2025/01/29/20250129-leavenworth",
	} {
		if !exists(filepath.Join(lib, rel)) {
			t.Errorf("%s disturbed by migrate", rel)
		}
	}
	// Directories the migration emptied are gone; ones with leftovers stay.
	if exists(filepath.Join(lib, "2026/02")) && func() bool { e, _ := os.ReadDir(filepath.Join(lib, "2026/02")); return len(e) == 0 }() {
		t.Error("emptied 2026/02 left behind")
	}
	if !exists(filepath.Join(lib, "2025/12/06/Stills")) == false {
		// Stills held only the one file, so it should be gone.
		t.Error("emptied Stills dir left behind")
	}
	if !exists(filepath.Join(lib, "2024/01/28")) { // still has .DS_Store
		t.Error("directory with junk was removed")
	}
	a, _ := f.env.Catalog.Asset(ctx, ids["2026/02/L1000598.MOV"])
	if a.RelPath != "2026/02/08/l1000598-"+a.ID+".mov" || a.CaptureTime.Day() != 8 {
		t.Errorf("month-only file: %+v", a)
	}
	if bad, err := f.env.Relink(ctx); err != nil || len(bad.Unavailable) != 0 {
		t.Errorf("relink after migrate: %+v, %v", bad, err)
	}

	// A card holding the same clip is recognised, not re-imported.
	card := filepath.Join(f.base, "card")
	os.MkdirAll(card, 0o755)
	src, _ := os.ReadFile(filepath.Join(lib, base+".braw"))
	os.WriteFile(filepath.Join(card, "A005_12062359_C001.braw"), src, 0o644)
	sum, err := f.env.Import(ctx, card)
	if err != nil || len(sum.Source.New) != 0 || sum.Spooled != 0 || sum.Archived != 0 || !sum.SafeToFormat {
		t.Errorf("import of adopted content: %+v, %v", sum, err)
	}
}

func TestAdoptRefusals(t *testing.T) {
	f := libraryFixture(t)
	if _, err := f.env.Adopt(ctx, "nope", AdoptOptions{}); err == nil {
		t.Error("unknown location accepted")
	}
	os.RemoveAll(f.spool)
	if _, err := f.env.Adopt(ctx, "fast", AdoptOptions{}); err == nil {
		t.Error("unmounted location accepted")
	}
	// A location with no tree directories yet is fine and does nothing.
	rep, err := f.env.Adopt(ctx, "slow", AdoptOptions{})
	if err != nil || len(rep.Actions) != 0 {
		t.Errorf("empty location: %+v, %v", rep, err)
	}
}

func TestDirDateAndLegacy(t *testing.T) {
	for rel, want := range map[string][4]int{
		"2024/01/28/x.braw":        {2024, 1, 28, 3},
		"2026/02/x.mov":            {2026, 2, 0, 2},
		"2025/12/06/Stills/x.braw": {2025, 12, 6, 3},
		"x.braw":                   {0, 0, 0, 0},
		"1234/99/x.braw":           {0, 0, 0, 0},
		"2025/13/x.braw":           {2025, 0, 0, 1},
	} {
		y, m, d, depth := dirDate(rel)
		if y != want[0] || int(m) != want[1] || d != want[2] || depth != want[3] {
			t.Errorf("dirDate(%q) = %d %d %d %d, want %v", rel, y, m, d, depth, want)
		}
	}
	base, ts := splitLegacy("Video Assist_0001_2024-11-08-075613_3aa7717c1b1dedb9", time.UTC)
	if base != "Video Assist_0001" || !ts.Equal(time.Date(2024, 11, 8, 7, 56, 13, 0, time.UTC)) {
		t.Errorf("splitLegacy = %q, %v", base, ts)
	}
	if b, ts := splitLegacy("A003_11160746_C001", time.UTC); b != "A003_11160746_C001" || !ts.IsZero() {
		t.Errorf("non-legacy: %q %v", b, ts)
	}
	_ = catalog.RoleProxy
}
