package scan

import (
	"encoding/binary"
	"errors"
	"io"
	"time"
)

var errNoMVHD = errors.New("no mvhd atom")

// quicktimeEpoch is 1904-01-01T00:00:00Z, the origin for mvhd times.
var quicktimeEpoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)

// mp4CreationTime returns the movie header creation time of an ISOBMFF or
// QuickTime file, in UTC. It walks top-level atoms, so a moov after a
// multi-terabyte mdat costs one seek. BRAW uses the same atom structure.
func mp4CreationTime(r io.ReaderAt, size int64) (time.Time, error) {
	moovOff, moovEnd, err := findAtom(r, 0, size, "moov")
	if err != nil {
		return time.Time{}, err
	}
	off, end, err := findAtom(r, moovOff, moovEnd, "mvhd")
	if err != nil {
		return time.Time{}, err
	}
	var hdr [20]byte
	n := int64(len(hdr))
	if end-off < n {
		n = end - off
	}
	if n < 8 {
		return time.Time{}, errNoMVHD
	}
	if _, err := r.ReadAt(hdr[:n], off); err != nil {
		return time.Time{}, err
	}
	var secs uint64
	switch hdr[0] {
	case 0:
		secs = uint64(binary.BigEndian.Uint32(hdr[4:8]))
	case 1:
		if n < 12 {
			return time.Time{}, errNoMVHD
		}
		secs = binary.BigEndian.Uint64(hdr[4:12])
	default:
		return time.Time{}, errNoMVHD
	}
	if secs == 0 {
		return time.Time{}, errNoMVHD
	}
	t := quicktimeEpoch.Add(time.Duration(secs) * time.Second)
	if t.Year() < 2000 {
		return time.Time{}, errNoMVHD
	}
	return t, nil
}

// findAtom scans [start, end) for a box of the given type and returns the
// bounds of its payload.
func findAtom(r io.ReaderAt, start, end int64, typ string) (int64, int64, error) {
	off := start
	var hdr [16]byte
	for off+8 <= end {
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return 0, 0, err
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		hlen := int64(8)
		switch size {
		case 0:
			size = end - off
		case 1:
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return 0, 0, err
			}
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
			hlen = 16
		}
		if size < hlen || off+size > end {
			return 0, 0, errNoMVHD
		}
		if string(hdr[4:8]) == typ {
			return off + hlen, off + size, nil
		}
		off += size
	}
	return 0, 0, errNoMVHD
}
