package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scottlaird/mediamanager/media"
)

const full = `
catalog: ~/cat.db
timezone: America/Los_Angeles
trees:
  video: {link: ~/Video}
  still: {subdir: stills, link: /Users/scott/Pictures/Import}
locations:
  - name: nas
    kind: nas
    path: /Volumes/video/mediamanager
  - name: slow
    kind: spool
    path: /Volumes/Slow/mm
    priority: 2
  - name: fast
    kind: spool
    volume_uuid: 1234-ABCD
    path: mediamanager
    priority: 1
extensions:
  video: [BRAW, mxf]
concurrency:
  nas: 5
`

func TestParseFull(t *testing.T) {
	c, err := Parse([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if c.Catalog != filepath.Join(home, "cat.db") {
		t.Errorf("catalog = %q", c.Catalog)
	}
	if loc, _ := c.Location(); loc.String() != "America/Los_Angeles" {
		t.Errorf("location = %v", loc)
	}
	v, ok := c.Tree(media.Video)
	if !ok || v.Subdir != "video" || v.Link != filepath.Join(home, "Video") {
		t.Errorf("video tree = %+v, %v", v, ok)
	}
	s, _ := c.Tree(media.Still)
	if s.Subdir != "stills" {
		t.Errorf("still tree = %+v", s)
	}
	if _, ok := c.Tree(media.Audio); ok {
		t.Error("audio tree present without config")
	}
	spools := c.Spools()
	if len(spools) != 2 || spools[0].Name != "fast" || spools[1].Name != "slow" {
		t.Errorf("spools = %+v", spools)
	}
	if nas := c.NAS(); len(nas) != 1 || nas[0].Path != "/Volumes/video/mediamanager" {
		t.Errorf("nas = %+v", nas)
	}
	if c.Concurrency.PerSource != 1 || c.Concurrency.NAS != 5 {
		t.Errorf("concurrency = %+v", c.Concurrency)
	}
	cl := c.Classifier()
	if k, _ := cl.Classify("a.mxf"); k != media.Video {
		t.Error("custom video extension not applied")
	}
	if k, _ := cl.Classify("a.mp4"); k != media.Unknown {
		t.Error("custom video extensions did not replace the defaults")
	}
	if k, _ := cl.Classify("a.dng"); k != media.Still {
		t.Error("default still extensions lost")
	}
}

func TestParseErrors(t *testing.T) {
	base := func(mod func(s string) string) string { return mod(full) }
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"no trees", "locations: [{name: n, kind: nas, path: /n}]", "no trees"},
		{"bad tree kind", "trees: {movies: {link: /x}}\nlocations: [{name: n, kind: nas, path: /n}]", "kind must be"},
		{"tree without link", "trees: {video: {}}\nlocations: [{name: n, kind: nas, path: /n}]", "link is required"},
		{"relative link", "trees: {video: {link: rel}}\nlocations: [{name: n, kind: nas, path: /n}]", "must be absolute"},
		{"nested subdir", "trees: {video: {subdir: a/b, link: /x}}\nlocations: [{name: n, kind: nas, path: /n}]", "single directory"},
		{"parent subdir", "trees: {video: {subdir: .., link: /x}}\nlocations: [{name: n, kind: nas, path: /n}]", "single directory"},
		{"no locations", "trees: {video: {link: /x}}", "no locations"},
		{"dup names", base(func(s string) string { return strings.Replace(s, "name: slow", "name: nas", 1) }), "duplicate"},
		{"bad kind", base(func(s string) string { return strings.Replace(s, "kind: nas", "kind: tape", 1) }), "spool or nas"},
		{"relative path no uuid", base(func(s string) string { return strings.Replace(s, "path: /Volumes/Slow/mm", "path: mm", 1) }), "must be absolute unless"},
		{"absolute path with uuid", base(func(s string) string { return strings.Replace(s, "path: mediamanager", "path: /mediamanager", 1) }), "relative to the volume"},
		{"bad zone", base(func(s string) string { return strings.Replace(s, "America/Los_Angeles", "Mars/Olympus", 1) }), "timezone"},
		{"not yaml", "trees: [", ""},
	}
	for _, tt := range tests {
		_, err := Parse([]byte(tt.yaml))
		if err == nil {
			t.Errorf("%s: no error", tt.name)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error %q does not mention %q", tt.name, err, tt.want)
		}
	}
}

func TestRootSubdir(t *testing.T) {
	c, err := Parse([]byte("trees: {video: {subdir: ., link: /v}}\nlocations: [{name: n, kind: nas, path: /n}]"))
	if err != nil {
		t.Fatal(err)
	}
	if tr, _ := c.Tree(media.Video); tr.Subdir != "." {
		t.Errorf("subdir = %q", tr.Subdir)
	}
}

func TestTemporalDefaults(t *testing.T) {
	base := "trees: {video: {link: /v}}\nlocations: [{name: n, kind: nas, path: /n}]\n"
	c, _ := Parse([]byte(base))
	if c.Temporal != nil {
		t.Error("temporal configured by default")
	}
	c, err := Parse([]byte(base + "temporal: {}"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Temporal == nil || c.Temporal.Address != "localhost:7233" || c.Temporal.Namespace != "default" || c.Temporal.TaskQueue != "mediamanager" {
		t.Errorf("temporal = %+v", c.Temporal)
	}
	c, _ = Parse([]byte(base + "temporal: {address: temporal.lan:7233, task_queue: mm}"))
	if c.Temporal.Address != "temporal.lan:7233" || c.Temporal.TaskQueue != "mm" || c.Temporal.Namespace != "default" {
		t.Errorf("temporal = %+v", c.Temporal)
	}
}

func TestDefaults(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/data")
	t.Setenv("XDG_CONFIG_HOME", "/conf")
	c, err := Parse([]byte("trees: {audio: {link: /a}}\nlocations: [{name: n, kind: nas, path: /n}]"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Catalog != "/data/mediamanager/catalog.db" {
		t.Errorf("catalog = %q", c.Catalog)
	}
	if c.Timezone != "Local" || c.Concurrency.PerSource != 1 || c.Concurrency.NAS != 3 {
		t.Errorf("defaults = %+v", c)
	}
	if DefaultPath() != "/conf/mediamanager/config.yaml" {
		t.Errorf("DefaultPath = %q", DefaultPath())
	}
	if k, _ := c.Classifier().Classify("a.braw"); k != media.Video {
		t.Error("default extensions missing")
	}
}

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte(full), 0o644)
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
}

func TestLocationRoot(t *testing.T) {
	find := func(uuid string) (string, error) {
		if uuid == "1234-ABCD" {
			return "/Volumes/Fast 1", nil
		}
		return "", ErrNotMounted
	}
	tests := []struct {
		name string
		loc  Location
		want string
		err  error
	}{
		{"absolute", Location{Name: "slow", Path: "/Volumes/Slow/mm"}, "/Volumes/Slow/mm", nil},
		{"by uuid", Location{Name: "fast", VolumeUUID: "1234-ABCD", Path: "mediamanager"}, "/Volumes/Fast 1/mediamanager", nil},
		{"uuid at root", Location{Name: "fast", VolumeUUID: "1234-ABCD"}, "/Volumes/Fast 1", nil},
		{"not mounted", Location{Name: "gone", VolumeUUID: "0000", Path: "x"}, "", ErrNotMounted},
	}
	for _, tt := range tests {
		got, err := tt.loc.Root(find)
		if !errors.Is(err, tt.err) || got != tt.want {
			t.Errorf("%s: got %q, %v; want %q, %v", tt.name, got, err, tt.want, tt.err)
		}
	}
	if _, err := (Location{VolumeUUID: "x"}).Root(nil); err == nil {
		t.Error("nil resolver accepted")
	}
}
