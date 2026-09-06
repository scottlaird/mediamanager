// Package config reads mediamanager's YAML settings.
//
// A location is a spool or NAS root. It can be named by absolute path, or
// by volume UUID plus a path relative to wherever that volume is mounted,
// which is the form to use for anything that lives under /Volumes on
// macOS: a name clash there mounts the disk as "Name 1" and an absolute
// path would quietly point at the wrong one.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/scottlaird/mediamanager/media"
	"github.com/scottlaird/mediamanager/scan"
	"go.yaml.in/yaml/v4"
)

// Config is the whole settings file.
type Config struct {
	// Catalog is the SQLite path. Default: $XDG_DATA_HOME/mediamanager/catalog.db.
	Catalog string `yaml:"catalog"`
	// Timezone names the zone capture times are interpreted in: an IANA
	// name, or "Local" (the default).
	Timezone string `yaml:"timezone"`
	// Trees is keyed by kind: video, audio, still. A kind with no tree is
	// reported and skipped at import.
	Trees       map[string]Tree `yaml:"trees"`
	Locations   []Location      `yaml:"locations"`
	Extensions  Extensions      `yaml:"extensions"`
	Concurrency Concurrency     `yaml:"concurrency"`
}

// Tree is where one kind's files live inside every location, and where
// its link tree is.
type Tree struct {
	// Subdir is the directory under each location root. Defaults to the
	// kind's name.
	Subdir string `yaml:"subdir"`
	// Link is the absolute path of the link tree for this kind.
	Link string `yaml:"link"`
}

// Location is a spool or NAS root.
type Location struct {
	Name string `yaml:"name"`
	// Kind is "spool" or "nas".
	Kind string `yaml:"kind"`
	// Path is absolute, or relative to the volume's mount point when
	// VolumeUUID is set.
	Path       string `yaml:"path"`
	VolumeUUID string `yaml:"volume_uuid"`
	// Priority orders spools for link resolution; lower wins. Ignored for
	// the NAS.
	Priority int `yaml:"priority"`
}

// Extensions override the default per-kind extension lists.
type Extensions struct {
	Video []string `yaml:"video"`
	Audio []string `yaml:"audio"`
	Still []string `yaml:"still"`
}

// Concurrency caps simultaneous copies.
type Concurrency struct {
	// PerSource is copies read from one source device at once. Default 1.
	PerSource int `yaml:"per_source"`
	// NAS is copies written to the NAS at once, system-wide. Default 3.
	NAS int `yaml:"nas"`
}

// DefaultPath is where Load looks when given no path.
func DefaultPath() string {
	return filepath.Join(configHome(), "mediamanager", "config.yaml")
}

// Load reads and validates the file at path, or DefaultPath when empty.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes YAML, fills defaults and validates.
func Parse(b []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.finish(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

func (c *Config) finish() error {
	if c.Catalog == "" {
		c.Catalog = filepath.Join(dataHome(), "mediamanager", "catalog.db")
	}
	c.Catalog = expand(c.Catalog)
	if c.Timezone == "" {
		c.Timezone = "Local"
	}
	if _, err := c.Location(); err != nil {
		return err
	}
	if c.Concurrency.PerSource <= 0 {
		c.Concurrency.PerSource = 1
	}
	if c.Concurrency.NAS <= 0 {
		c.Concurrency.NAS = 3
	}
	if len(c.Extensions.Video) == 0 {
		c.Extensions.Video = scan.DefaultVideoExts
	}
	if len(c.Extensions.Audio) == 0 {
		c.Extensions.Audio = scan.DefaultAudioExts
	}
	if len(c.Extensions.Still) == 0 {
		c.Extensions.Still = scan.DefaultStillExts
	}

	if len(c.Trees) == 0 {
		return errors.New("no trees configured")
	}
	for name, t := range c.Trees {
		if kindOf(name) == media.Unknown {
			return fmt.Errorf("tree %q: kind must be video, audio or still", name)
		}
		if t.Subdir == "" {
			t.Subdir = name
		}
		if strings.Contains(t.Subdir, "/") || t.Subdir == "." || t.Subdir == ".." {
			return fmt.Errorf("tree %q: subdir must be a single directory name", name)
		}
		if t.Link == "" {
			return fmt.Errorf("tree %q: link is required", name)
		}
		t.Link = expand(t.Link)
		if !filepath.IsAbs(t.Link) {
			return fmt.Errorf("tree %q: link must be absolute", name)
		}
		c.Trees[name] = t
	}

	if len(c.Locations) == 0 {
		return errors.New("no locations configured")
	}
	seen := map[string]bool{}
	for i := range c.Locations {
		l := &c.Locations[i]
		if l.Name == "" {
			return fmt.Errorf("location %d: name is required", i)
		}
		if seen[l.Name] {
			return fmt.Errorf("location %q: duplicate name", l.Name)
		}
		seen[l.Name] = true
		if l.Kind != "spool" && l.Kind != "nas" {
			return fmt.Errorf("location %q: kind must be spool or nas", l.Name)
		}
		if l.Path == "" && l.VolumeUUID == "" {
			return fmt.Errorf("location %q: path is required", l.Name)
		}
		l.Path = expand(l.Path)
		if l.VolumeUUID == "" && !filepath.IsAbs(l.Path) {
			return fmt.Errorf("location %q: path must be absolute unless volume_uuid is set", l.Name)
		}
		if l.VolumeUUID != "" && filepath.IsAbs(l.Path) {
			return fmt.Errorf("location %q: path must be relative to the volume when volume_uuid is set", l.Name)
		}
	}
	sort.SliceStable(c.Locations, func(i, j int) bool { return c.Locations[i].Priority < c.Locations[j].Priority })
	return nil
}

// Location returns the zone named by Timezone.
func (c *Config) Location() (*time.Location, error) {
	if c.Timezone == "Local" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil, fmt.Errorf("timezone %q: %w", c.Timezone, err)
	}
	return loc, nil
}

// Classifier builds the extension classifier from Extensions.
func (c *Config) Classifier() *scan.Classifier {
	return scan.NewClassifier(c.Extensions.Video, c.Extensions.Audio, c.Extensions.Still)
}

// Tree returns the tree for a kind, if one is configured.
func (c *Config) Tree(k media.Kind) (Tree, bool) {
	t, ok := c.Trees[k.String()]
	return t, ok
}

// Spools returns spool locations best priority first.
func (c *Config) Spools() []Location { return c.byKind("spool") }

// NAS returns NAS locations.
func (c *Config) NAS() []Location { return c.byKind("nas") }

func (c *Config) byKind(kind string) []Location {
	var out []Location
	for _, l := range c.Locations {
		if l.Kind == kind {
			out = append(out, l)
		}
	}
	return out
}

// ErrNotMounted is what a mount resolver returns for a volume that is not
// currently present. Callers skip the location for the run; they never
// treat it as empty.
var ErrNotMounted = errors.New("volume not mounted")

// Root resolves the location's root directory for this run. find maps a
// volume UUID to its current mount point and is only consulted when
// VolumeUUID is set; volume.Find is the usual argument.
func (l Location) Root(find func(uuid string) (string, error)) (string, error) {
	if l.VolumeUUID == "" {
		return l.Path, nil
	}
	if find == nil {
		return "", errors.New("config: no volume resolver")
	}
	mount, err := find(l.VolumeUUID)
	if err != nil {
		return "", fmt.Errorf("location %q: %w", l.Name, err)
	}
	return filepath.Join(mount, l.Path), nil
}

func kindOf(name string) media.Kind {
	switch name {
	case "video":
		return media.Video
	case "audio":
		return media.Audio
	case "still":
		return media.Still
	}
	return media.Unknown
}

// expand replaces a leading ~ with the home directory.
func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

func configHome() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return x
	}
	return expand("~/.config")
}

func dataHome() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return x
	}
	return expand("~/.local/share")
}
