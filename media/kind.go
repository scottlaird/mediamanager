// Package media defines the kinds of media file mediamanager handles.
package media

import "fmt"

// Kind classifies an original file. Routing between the video, audio and
// still trees is by Kind, never by the device the file came from.
type Kind int

const (
	Unknown Kind = iota
	Video
	Audio
	Still
)

func (k Kind) String() string {
	switch k {
	case Video:
		return "video"
	case Audio:
		return "audio"
	case Still:
		return "still"
	default:
		return fmt.Sprintf("Kind(%d)", int(k))
	}
}

// UsesSparseID reports whether files of this kind are identified by the
// sparse checksum (large, immutable video and audio) rather than a full
// content hash.
func (k Kind) UsesSparseID() bool {
	return k == Video || k == Audio
}
