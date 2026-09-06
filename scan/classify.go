package scan

import (
	"strings"

	"github.com/scottlaird/mediamanager/media"
)

// Classifier maps file extensions to media kinds. Matching is
// case-insensitive; the tables are the user's to extend.
type Classifier struct {
	byExt map[string]media.Kind
}

// Default extension tables. HEIC/HEIF are stills even though the container
// is ISOBMFF; their capture time comes from mtime for now.
var (
	DefaultVideoExts = []string{"braw", "mov", "mp4"}
	DefaultAudioExts = []string{"wav", "flac", "mp3"}
	DefaultStillExts = []string{"jpg", "jpeg", "heif", "heic", "dng", "rw2", "3fr", "tif", "tiff"}
)

// NewClassifier builds a classifier from extension lists, without dots.
// A later list wins if an extension appears twice.
func NewClassifier(video, audio, still []string) *Classifier {
	c := &Classifier{byExt: map[string]media.Kind{}}
	for kind, exts := range map[media.Kind][]string{media.Video: video, media.Audio: audio, media.Still: still} {
		for _, e := range exts {
			c.byExt[strings.ToLower(strings.TrimPrefix(e, "."))] = kind
		}
	}
	return c
}

// DefaultClassifier uses the Default*Exts tables.
func DefaultClassifier() *Classifier {
	return NewClassifier(DefaultVideoExts, DefaultAudioExts, DefaultStillExts)
}

// Classify returns the kind for a basename and its lowercase extension.
// Unrecognised names return media.Unknown.
func (c *Classifier) Classify(name string) (media.Kind, string) {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return media.Unknown, ""
	}
	ext := strings.ToLower(name[i+1:])
	return c.byExt[ext], ext
}
