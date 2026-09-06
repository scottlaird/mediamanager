package scan

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/scottlaird/mediamanager/media"
)

// mkTree creates files (relative paths, slash-separated) under root with a
// little content each.
func mkTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func rels(files []File) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Rel)
	}
	return out
}

func TestScanDCIM(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root,
		"DCIM/100_PANA/P1000123.MP4",
		"DCIM/100_PANA/P1000123.RW2",
		"DCIM/100_PANA/P1000124.JPG",
		"DCIM/100_PANA/._P1000123.MP4",
		"DCIM/100_PANA/P1000123.XML",
		"DCIM/.DS_Store",
		"DCIM/101_PANA/P1010001.RW2",
		"MISC/AUTPRINT.MRK",
		"PRIVATE/AVCHD/BDMV/STREAM/00001.MTS",
		".Spotlight-V100/store.db",
		"clip-at-root.mp4", // ignored on a DCIM card: only DCIM/ is scanned
	)
	res, err := Scan(root, DefaultClassifier())
	if err != nil {
		t.Fatal(err)
	}
	if res.Shape != DCIM {
		t.Errorf("shape = %v, want DCIM", res.Shape)
	}
	want := []string{
		"DCIM/100_PANA/P1000123.MP4",
		"DCIM/100_PANA/P1000123.RW2",
		"DCIM/100_PANA/P1000124.JPG",
		"DCIM/101_PANA/P1010001.RW2",
	}
	if got := rels(res.Files); !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
	if want := []string{"DCIM/100_PANA/P1000123.XML"}; !reflect.DeepEqual(res.Unrecognised, want) {
		t.Errorf("unrecognised = %v, want %v", res.Unrecognised, want)
	}
	f := res.Files[0]
	if f.Kind != media.Video || f.Ext != "mp4" || f.Size != int64(len(f.Rel)) || !f.IsOriginal() || f.Base() != "P1000123" {
		t.Errorf("file = %+v", f)
	}
	if res.Files[1].Kind != media.Still {
		t.Errorf("RW2 classified as %v", res.Files[1].Kind)
	}
}

func TestScanFlat(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root,
		"A001_09051412_C001.braw",
		"A001_09051412_C001.sidecar",
		"A001_09051412_C002.braw",
		"Proxy/A001_09051412_C001.mp4",
		"Proxy/A001_09051412_C001.sidecar",
		"Proxy/A001_09051412_C002.mp4",
		"Proxy/.DS_Store",
		"sub/reel2.MOV",
		"sub/proxy/reel2.mp4",
		".fseventsd/000001",
		".Trashes/501/old.braw",
		"notes.txt",
		"partial-copy.braw.partial",
	)
	res, err := Scan(root, DefaultClassifier())
	if err != nil {
		t.Fatal(err)
	}
	if res.Shape != Flat {
		t.Errorf("shape = %v, want Flat", res.Shape)
	}
	want := []string{
		"A001_09051412_C001.braw",
		"A001_09051412_C001.sidecar",
		"A001_09051412_C002.braw",
		"Proxy/A001_09051412_C001.mp4",
		"Proxy/A001_09051412_C001.sidecar",
		"Proxy/A001_09051412_C002.mp4",
		"sub/proxy/reel2.mp4",
		"sub/reel2.MOV",
	}
	if got := rels(res.Files); !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
	if want := []string{"notes.txt"}; !reflect.DeepEqual(res.Unrecognised, want) {
		t.Errorf("unrecognised = %v, want %v", res.Unrecognised, want)
	}
	byRel := map[string]File{}
	for _, f := range res.Files {
		byRel[f.Rel] = f
	}
	for rel, wantRole := range map[string]Role{
		"A001_09051412_C001.braw":          Original,
		"A001_09051412_C001.sidecar":       Sidecar,
		"Proxy/A001_09051412_C001.mp4":     Proxy,
		"Proxy/A001_09051412_C001.sidecar": ProxySidecar,
		"sub/proxy/reel2.mp4":              Proxy,
		"sub/reel2.MOV":                    Original,
	} {
		if byRel[rel].Role != wantRole {
			t.Errorf("%s: role = %q, want %q", rel, byRel[rel].Role, wantRole)
		}
	}
	if byRel["A001_09051412_C001.sidecar"].Kind != media.Unknown || byRel["A001_09051412_C001.sidecar"].Ext != "sidecar" {
		t.Errorf("sidecar file = %+v", byRel["A001_09051412_C001.sidecar"])
	}
	for _, rel := range []string{"Proxy/A001_09051412_C001.sidecar", "A001_09051412_C001.sidecar", "Proxy/A001_09051412_C001.mp4"} {
		if b := byRel[rel].Base(); b != "A001_09051412_C001" {
			t.Errorf("%s: base %q", rel, b)
		}
	}
	if d := byRel["Proxy/A001_09051412_C001.sidecar"].OriginalDir(); d != "." {
		t.Errorf("proxy sidecar OriginalDir = %q", d)
	}
	if d := byRel["sub/proxy/reel2.mp4"].OriginalDir(); d != "sub" {
		t.Errorf("OriginalDir = %q, want sub", d)
	}
	if d := byRel["Proxy/A001_09051412_C001.mp4"].OriginalDir(); d != "." {
		t.Errorf("OriginalDir = %q, want .", d)
	}
	if d := byRel["sub/reel2.MOV"].OriginalDir(); d != "sub" {
		t.Errorf("OriginalDir of original = %q, want sub", d)
	}
}

func TestScanMissingRoot(t *testing.T) {
	if _, err := Scan(filepath.Join(t.TempDir(), "nope"), DefaultClassifier()); err == nil {
		t.Error("scanning a missing root succeeded")
	}
}

func TestClassify(t *testing.T) {
	c := DefaultClassifier()
	tests := []struct {
		name string
		kind media.Kind
		ext  string
	}{
		{"A001.BRAW", media.Video, "braw"},
		{"clip.MoV", media.Video, "mov"},
		{"ZOOM0001.WAV", media.Audio, "wav"},
		{"L1004821.DNG", media.Still, "dng"},
		{"IMG_0001.HEIC", media.Still, "heic"},
		{"X2D_0001.3FR", media.Still, "3fr"},
		{"notes.txt", media.Unknown, "txt"},
		{"README", media.Unknown, ""},
		{".hidden", media.Unknown, ""},
		{"trailingdot.", media.Unknown, ""},
	}
	for _, tt := range tests {
		kind, ext := c.Classify(tt.name)
		if kind != tt.kind || ext != tt.ext {
			t.Errorf("Classify(%q) = %v, %q; want %v, %q", tt.name, kind, ext, tt.kind, tt.ext)
		}
	}
	custom := NewClassifier([]string{".MXF"}, nil, nil)
	if kind, _ := custom.Classify("a.mxf"); kind != media.Video {
		t.Errorf("custom classifier ignored .MXF")
	}
	if kind, _ := custom.Classify("a.mp4"); kind != media.Unknown {
		t.Errorf("custom classifier kept defaults")
	}
}

func TestIsJunk(t *testing.T) {
	for name, want := range map[string]bool{
		".DS_Store": true, "._clip.braw": true, ".Spotlight-V100": true, ".Trashes": true,
		"Thumbs.db": true, "clip.braw.partial": true,
		"clip.braw": false, ".hidden.mov": false, "_underscore.mp4": false, "Proxy": false,
	} {
		if got := IsJunk(name); got != want {
			t.Errorf("IsJunk(%q) = %v, want %v", name, got, want)
		}
	}
}
