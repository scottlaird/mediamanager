package naming

import (
	"testing"
	"time"

	"github.com/scottlaird/mediamanager/identity"
	"github.com/scottlaird/mediamanager/media"
)

const testID identity.ID = "3f9a1c2e7b4d5a60"

var shot = time.Date(2026, 9, 5, 22, 14, 0, 0, time.Local)

func TestFileName(t *testing.T) {
	tests := []struct {
		name    string
		a       Asset
		want    string
		wantErr bool
	}{
		{"video", Asset{media.Video, "A001_09051412_C003.BRAW", testID, shot}, "a001_09051412_c003-3f9a1c2e7b4d5a60.braw", false},
		{"audio", Asset{media.Audio, "ZOOM0012.WAV", testID, shot}, "zoom0012-3f9a1c2e7b4d5a60.wav", false},
		{"still", Asset{media.Still, "L1004821.DNG", "", shot}, "l1004821.dng", false},
		{"still ignores id", Asset{media.Still, "L1004821.DNG", testID, shot}, "l1004821.dng", false},
		{"already named, same id", Asset{media.Video, "a001-3f9a1c2e7b4d5a60.braw", testID, shot}, "a001-3f9a1c2e7b4d5a60.braw", false},
		{"already named, other id", Asset{media.Video, "a001-0000000000000000.braw", testID, shot}, "a001-0000000000000000-3f9a1c2e7b4d5a60.braw", false},
		{"odd characters", Asset{media.Video, "Clip (1) é.mov", testID, shot}, "clip__1___-3f9a1c2e7b4d5a60.mov", false},
		{"no extension", Asset{media.Video, "CLIP", testID, shot}, "clip-3f9a1c2e7b4d5a60", false},
		{"dotfile", Asset{media.Still, ".hidden", "", shot}, ".hidden", false},
		{"missing id", Asset{media.Video, "A.mov", "", shot}, "", true},
		{"bad id", Asset{media.Video, "A.mov", "XYZ", shot}, "", true},
		{"no time", Asset{media.Video, "A.mov", testID, time.Time{}}, "", true},
		{"empty name", Asset{media.Video, "", testID, shot}, "", true},
	}
	for _, tt := range tests {
		got, err := FileName(tt.a)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tt.name, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestSchemes(t *testing.T) {
	a := Asset{media.Video, "A001_09051412_C003.BRAW", testID, shot}
	tests := []struct {
		name    string
		s       Scheme
		want    string
		wantErr bool
	}{
		{"date", DateScheme{}, "2026/09/05/a001_09051412_c003-3f9a1c2e7b4d5a60.braw", false},
		{"project", ProjectScheme{"Boat Launch"}, "2026/20260905-boat-launch/a001_09051412_c003-3f9a1c2e7b4d5a60.braw", false},
		{"project odd chars", ProjectScheme{" Trip/Two "}, "2026/20260905-trip_two/a001_09051412_c003-3f9a1c2e7b4d5a60.braw", false},
		{"project empty", ProjectScheme{"  "}, "", true},
	}
	for _, tt := range tests {
		got, err := tt.s.Path(a)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tt.name, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestSchemesArePure(t *testing.T) {
	a := Asset{media.Still, "L1004821.DNG", "", shot}
	first, _ := DateScheme{}.Path(a)
	for i := 0; i < 3; i++ {
		if got, _ := (DateScheme{}).Path(a); got != first {
			t.Fatalf("path changed between calls: %q then %q", first, got)
		}
	}
}

func TestProxyPath(t *testing.T) {
	tests := []struct {
		rel, ext, want string
	}{
		{"2026/09/05/a001-3f9a1c2e7b4d5a60.braw", "mp4", "2026/09/05/Proxy/a001-3f9a1c2e7b4d5a60.mp4"},
		{"a001-3f9a1c2e7b4d5a60.braw", "MOV", "Proxy/a001-3f9a1c2e7b4d5a60.mov"},
	}
	for _, tt := range tests {
		if got := ProxyPath(tt.rel, tt.ext); got != tt.want {
			t.Errorf("ProxyPath(%q, %q) = %q, want %q", tt.rel, tt.ext, got, tt.want)
		}
	}
}

func TestSidecarPaths(t *testing.T) {
	rel := "2026/09/05/a001-3f9a1c2e7b4d5a60.braw"
	if got := SidecarPath(rel); got != "2026/09/05/a001-3f9a1c2e7b4d5a60.sidecar" {
		t.Errorf("SidecarPath = %q", got)
	}
	if got := ProxySidecarPath(rel); got != "2026/09/05/Proxy/a001-3f9a1c2e7b4d5a60.sidecar" {
		t.Errorf("ProxySidecarPath = %q", got)
	}
}

func TestWithSuffix(t *testing.T) {
	tests := []struct {
		rel  string
		n    int
		want string
	}{
		{"2026/09/05/l1004821.dng", 1, "2026/09/05/l1004821_1.dng"},
		{"l1004821.dng", 12, "l1004821_12.dng"},
		{"noext", 1, "noext_1"},
	}
	for _, tt := range tests {
		if got := WithSuffix(tt.rel, tt.n); got != tt.want {
			t.Errorf("WithSuffix(%q, %d) = %q, want %q", tt.rel, tt.n, got, tt.want)
		}
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		want Parsed
		ok   bool
	}{
		{"a001_09051412_c003-3f9a1c2e7b4d5a60.braw", Parsed{"a001_09051412_c003", testID, "braw"}, true},
		{"clip-3f9a1c2e7b4d5a60", Parsed{"clip", testID, ""}, true},
		{"l1004821.dng", Parsed{}, false},
		{"legacy-name-with-dashes.mov", Parsed{}, false},
		{"-3f9a1c2e7b4d5a60.mov", Parsed{}, false},
		{"x-3F9A1C2E7B4D5A60.mov", Parsed{}, false},
		{"x-3f9a1c2e7b4d5a6.mov", Parsed{}, false},
	}
	for _, tt := range tests {
		got, ok := Parse(tt.name)
		if ok != tt.ok || got != tt.want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func TestFileNameRoundTripsThroughParse(t *testing.T) {
	a := Asset{media.Video, "A001_09051412_C003.BRAW", testID, shot}
	name, err := FileName(a)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := Parse(name)
	if !ok || p.ID != testID || p.Ext != "braw" || p.Base != "a001_09051412_c003" {
		t.Errorf("Parse(%q) = %+v, %v", name, p, ok)
	}
}
