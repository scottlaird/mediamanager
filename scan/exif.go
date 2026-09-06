package scan

import (
	"encoding/binary"
	"errors"
	"io"
	"time"
)

// A one-tag EXIF reader: enough TIFF to find DateTimeOriginal in a JPEG's
// APP1 segment or in a TIFF-based raw (DNG, RW2, 3FR, TIFF), and nothing
// more. Reads are small and on demand, so a 200 MB raw costs a few KB.

const (
	tagDateTime          = 0x0132
	tagExifIFD           = 0x8769
	tagDateTimeOriginal  = 0x9003
	tagDateTimeDigitized = 0x9004
	typeASCII            = 2
	maxIFDEntries        = 2000
)

var errNoEXIF = errors.New("no EXIF date")

// exifCaptureTime returns DateTimeOriginal (or, failing that,
// DateTimeDigitized, then IFD0 DateTime) interpreted in loc. EXIF times
// carry no zone; they are the camera's wall clock.
func exifCaptureTime(r io.ReaderAt, loc *time.Location) (time.Time, error) {
	tiff, err := findTIFF(r)
	if err != nil {
		return time.Time{}, err
	}
	t := &tiffReader{r: tiff}
	if err := t.header(); err != nil {
		return time.Time{}, err
	}
	ifd0, err := t.readIFD(t.ifd0)
	if err != nil {
		return time.Time{}, err
	}
	candidates := []string{}
	if off, ok := t.long(ifd0, tagExifIFD); ok {
		if exif, err := t.readIFD(int64(off)); err == nil {
			if s, ok := t.ascii(exif, tagDateTimeOriginal); ok {
				candidates = append(candidates, s)
			}
			if s, ok := t.ascii(exif, tagDateTimeDigitized); ok {
				candidates = append(candidates, s)
			}
		}
	}
	if s, ok := t.ascii(ifd0, tagDateTime); ok {
		candidates = append(candidates, s)
	}
	for _, s := range candidates {
		if tm, err := time.ParseInLocation("2006:01:02 15:04:05", s, loc); err == nil && tm.Year() > 1970 {
			return tm, nil
		}
	}
	return time.Time{}, errNoEXIF
}

// findTIFF returns a ReaderAt positioned at the TIFF header: the file
// itself for TIFF-based raws, or the APP1 payload for JPEGs.
func findTIFF(r io.ReaderAt) (io.ReaderAt, error) {
	var head [4]byte
	if _, err := r.ReadAt(head[:], 0); err != nil {
		return nil, err
	}
	switch {
	case head[0] == 'I' && head[1] == 'I', head[0] == 'M' && head[1] == 'M':
		return r, nil
	case head[0] == 0xFF && head[1] == 0xD8:
		return jpegAPP1(r)
	}
	return nil, errNoEXIF
}

func jpegAPP1(r io.ReaderAt) (io.ReaderAt, error) {
	off := int64(2)
	var hdr [4]byte
	for i := 0; i < 64; i++ {
		if _, err := r.ReadAt(hdr[:], off); err != nil {
			return nil, errNoEXIF
		}
		if hdr[0] != 0xFF {
			return nil, errNoEXIF
		}
		marker := hdr[1]
		if marker == 0xDA || marker == 0xD9 { // start of scan / end of image
			return nil, errNoEXIF
		}
		length := int64(binary.BigEndian.Uint16(hdr[2:]))
		if marker == 0xE1 {
			var sig [6]byte
			if _, err := r.ReadAt(sig[:], off+4); err == nil && string(sig[:]) == "Exif\x00\x00" {
				return io.NewSectionReader(r, off+10, length-8), nil
			}
		}
		off += 2 + length
	}
	return nil, errNoEXIF
}

type tiffReader struct {
	r    io.ReaderAt
	bo   binary.ByteOrder
	ifd0 int64
}

func (t *tiffReader) header() error {
	var h [8]byte
	if _, err := t.r.ReadAt(h[:], 0); err != nil {
		return err
	}
	switch string(h[:2]) {
	case "II":
		t.bo = binary.LittleEndian
	case "MM":
		t.bo = binary.BigEndian
	default:
		return errNoEXIF
	}
	// 0x2A is TIFF; 0x55 is Panasonic's RW2 variant of the same layout.
	if magic := t.bo.Uint16(h[2:]); magic != 0x2A && magic != 0x55 {
		return errNoEXIF
	}
	t.ifd0 = int64(t.bo.Uint32(h[4:]))
	return nil
}

type ifdEntry struct {
	typ   uint16
	count uint32
	raw   [4]byte
}

type ifd map[uint16]ifdEntry

func (t *tiffReader) readIFD(off int64) (ifd, error) {
	var cnt [2]byte
	if _, err := t.r.ReadAt(cnt[:], off); err != nil {
		return nil, err
	}
	n := int(t.bo.Uint16(cnt[:]))
	if n == 0 || n > maxIFDEntries {
		return nil, errNoEXIF
	}
	buf := make([]byte, 12*n)
	if _, err := t.r.ReadAt(buf, off+2); err != nil {
		return nil, err
	}
	out := ifd{}
	for i := 0; i < n; i++ {
		e := buf[12*i:]
		var ent ifdEntry
		ent.typ = t.bo.Uint16(e[2:])
		ent.count = t.bo.Uint32(e[4:])
		copy(ent.raw[:], e[8:12])
		out[t.bo.Uint16(e[0:])] = ent
	}
	return out, nil
}

func (t *tiffReader) long(d ifd, tag uint16) (uint32, bool) {
	e, ok := d[tag]
	if !ok {
		return 0, false
	}
	return t.bo.Uint32(e.raw[:]), true
}

func (t *tiffReader) ascii(d ifd, tag uint16) (string, bool) {
	e, ok := d[tag]
	if !ok || e.typ != typeASCII || e.count == 0 || e.count > 64 {
		return "", false
	}
	var b []byte
	if e.count <= 4 {
		b = e.raw[:e.count]
	} else {
		b = make([]byte, e.count)
		if _, err := t.r.ReadAt(b, int64(t.bo.Uint32(e.raw[:]))); err != nil {
			return "", false
		}
	}
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return string(b), true
}
