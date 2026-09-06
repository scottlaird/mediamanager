package scan

import (
	"os"
	"time"

	"github.com/scottlaird/mediamanager/media"
)

// TimeSource says where a capture time came from.
type TimeSource int

const (
	FromModTime TimeSource = iota
	FromEXIF
	FromMP4
)

func (s TimeSource) String() string {
	switch s {
	case FromEXIF:
		return "exif"
	case FromMP4:
		return "mp4"
	default:
		return "mtime"
	}
}

// CaptureTime returns when f was recorded, expressed in loc. Stills use
// EXIF DateTimeOriginal (the camera's wall clock, taken as loc time); video
// and audio use the container's creation time when it has one (UTC,
// converted to loc); anything else, and any read failure, falls back to
// the file's mtime. Metadata reads are a few small ReadAts.
func CaptureTime(f File, loc *time.Location) (time.Time, TimeSource) {
	if loc == nil {
		loc = time.Local
	}
	fallback := f.ModTime.In(loc)
	fh, err := os.Open(f.Abs)
	if err != nil {
		return fallback, FromModTime
	}
	defer fh.Close()

	switch {
	case f.Kind == media.Still && tiffLike[f.Ext]:
		if t, err := exifCaptureTime(fh, loc); err == nil {
			return t, FromEXIF
		}
	case f.Kind == media.Video && isobmff[f.Ext]:
		if t, err := mp4CreationTime(fh, f.Size); err == nil {
			return t.In(loc), FromMP4
		}
	}
	return fallback, FromModTime
}

var (
	tiffLike = map[string]bool{"jpg": true, "jpeg": true, "tif": true, "tiff": true, "dng": true, "rw2": true, "3fr": true}
	isobmff  = map[string]bool{"mp4": true, "mov": true, "braw": true}
)
