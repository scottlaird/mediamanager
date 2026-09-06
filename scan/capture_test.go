package scan

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scottlaird/mediamanager/media"
)

var pdt = time.FixedZone("PDT", -7*3600)

// tiffWithDates builds a minimal TIFF: IFD0 with DateTime and an Exif IFD
// pointer, then the Exif IFD with DateTimeOriginal. magic lets RW2's 0x55
// variant be exercised.
func tiffWithDates(bo binary.ByteOrder, magic uint16, ifd0Date, original string) []byte {
	var b bytes.Buffer
	put16 := func(v uint16) { binary.Write(&b, bo, v) }
	put32 := func(v uint32) { binary.Write(&b, bo, v) }
	if bo == binary.LittleEndian {
		b.WriteString("II")
	} else {
		b.WriteString("MM")
	}
	put16(magic)
	put32(8) // IFD0 at 8
	// Layout: IFD0 (2 + 2*12 + 4 = 30 bytes) at 8..38, then strings, then Exif IFD.
	ifd0Off := uint32(8)
	dateOff := ifd0Off + 30
	exifOff := dateOff + 20
	origOff := exifOff + 2 + 12 + 4
	// IFD0
	put16(2)
	put16(tagDateTime)
	put16(typeASCII)
	put32(20)
	put32(dateOff)
	put16(tagExifIFD)
	put16(4) // LONG
	put32(1)
	put32(exifOff)
	put32(0)
	b.WriteString(ifd0Date + "\x00")
	// Exif IFD
	put16(1)
	put16(tagDateTimeOriginal)
	put16(typeASCII)
	put32(20)
	put32(origOff)
	put32(0)
	b.WriteString(original + "\x00")
	return b.Bytes()
}

func jpegWrapping(tiff []byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{0xFF, 0xD8})
	// A COM segment first, to prove the walker skips segments.
	b.Write([]byte{0xFF, 0xFE, 0x00, 0x05, 'h', 'i', '!'})
	payload := append([]byte("Exif\x00\x00"), tiff...)
	b.Write([]byte{0xFF, 0xE1})
	binary.Write(&b, binary.BigEndian, uint16(len(payload)+2))
	b.Write(payload)
	b.Write([]byte{0xFF, 0xDA, 0x00, 0x02}) // SOS, then nothing useful
	return b.Bytes()
}

func atom(typ string, payload ...[]byte) []byte {
	var body []byte
	for _, p := range payload {
		body = append(body, p...)
	}
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(8+len(body)))
	b.WriteString(typ)
	b.Write(body)
	return b.Bytes()
}

func largeAtom(typ string, body []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(1))
	b.WriteString(typ)
	binary.Write(&b, binary.BigEndian, uint64(16+len(body)))
	b.Write(body)
	return b.Bytes()
}

func mvhd(version byte, created time.Time) []byte {
	secs := uint64(created.Sub(quicktimeEpoch) / time.Second)
	var b bytes.Buffer
	b.WriteByte(version)
	b.Write([]byte{0, 0, 0})
	if version == 1 {
		binary.Write(&b, binary.BigEndian, secs)
		binary.Write(&b, binary.BigEndian, secs) // modification
	} else {
		binary.Write(&b, binary.BigEndian, uint32(secs))
		binary.Write(&b, binary.BigEndian, uint32(secs))
	}
	b.Write(make([]byte, 80)) // rest of the header, irrelevant here
	return b.Bytes()
}

func writeFile(t *testing.T, dir, name string, b []byte, mtime time.Time) File {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	kind, ext := DefaultClassifier().Classify(name)
	return File{Rel: name, Abs: p, Kind: kind, Ext: ext, Size: int64(len(b)), ModTime: mtime}
}

func TestCaptureTime(t *testing.T) {
	dir := t.TempDir()
	mtime := time.Date(2026, 9, 6, 3, 30, 0, 0, time.UTC) // 2026-09-05 20:30 PDT
	shotWall := "2026:09:05 20:14:33"
	wantEXIF := time.Date(2026, 9, 5, 20, 14, 33, 0, pdt)
	created := time.Date(2026, 9, 6, 3, 14, 33, 0, time.UTC) // same instant as wantEXIF

	tiffLE := tiffWithDates(binary.LittleEndian, 0x2A, "2026:09:05 21:00:00", shotWall)
	tiffBE := tiffWithDates(binary.BigEndian, 0x2A, "2026:09:05 21:00:00", shotWall)
	rw2 := tiffWithDates(binary.LittleEndian, 0x55, "2026:09:05 21:00:00", shotWall)
	ifd0Only := tiffWithDates(binary.LittleEndian, 0x2A, shotWall, "")

	mp4Front := append(append(atom("ftyp", []byte("isom")), atom("moov", atom("mvhd", mvhd(0, created)))...), atom("mdat", make([]byte, 100))...)
	mp4Back := append(append(atom("ftyp", []byte("isom")), largeAtom("mdat", make([]byte, 100))...), atom("moov", atom("trak"), atom("mvhd", mvhd(1, created)))...)
	mp4Zero := append(atom("ftyp", []byte("isom")), atom("moov", atom("mvhd", mvhd(0, quicktimeEpoch)))...)

	tests := []struct {
		name   string
		file   File
		want   time.Time
		source TimeSource
	}{
		{"dng little endian", writeFile(t, dir, "a.dng", tiffLE, mtime), wantEXIF, FromEXIF},
		{"tif big endian", writeFile(t, dir, "b.tif", tiffBE, mtime), wantEXIF, FromEXIF},
		{"rw2 magic", writeFile(t, dir, "c.rw2", rw2, mtime), wantEXIF, FromEXIF},
		{"jpeg app1", writeFile(t, dir, "d.jpg", jpegWrapping(tiffLE), mtime), wantEXIF, FromEXIF},
		{"ifd0 datetime fallback", writeFile(t, dir, "e.dng", ifd0Only, mtime), wantEXIF, FromEXIF},
		{"heic uses mtime", writeFile(t, dir, "f.heic", []byte("not parsed"), mtime), mtime.In(pdt), FromModTime},
		{"garbage jpeg", writeFile(t, dir, "g.jpg", []byte{0xFF, 0xD8, 0xFF, 0xD9}, mtime), mtime.In(pdt), FromModTime},
		{"mp4 moov first v0", writeFile(t, dir, "h.mp4", mp4Front, mtime), created.In(pdt), FromMP4},
		{"mov moov last v1 large mdat", writeFile(t, dir, "i.mov", mp4Back, mtime), created.In(pdt), FromMP4},
		{"braw same walker", writeFile(t, dir, "j.braw", mp4Front, mtime), created.In(pdt), FromMP4},
		{"mp4 zero creation falls back", writeFile(t, dir, "k.mp4", mp4Zero, mtime), mtime.In(pdt), FromModTime},
		{"truncated mp4", writeFile(t, dir, "l.mp4", mp4Front[:10], mtime), mtime.In(pdt), FromModTime},
		{"wav uses mtime", writeFile(t, dir, "m.wav", []byte("RIFF"), mtime), mtime.In(pdt), FromModTime},
	}
	for _, tt := range tests {
		got, src := CaptureTime(tt.file, pdt)
		if !got.Equal(tt.want) || src != tt.source {
			t.Errorf("%s: got %v (%v), want %v (%v)", tt.name, got, src, tt.want, tt.source)
		}
		if got.Location() != pdt {
			t.Errorf("%s: result not in requested zone: %v", tt.name, got.Location())
		}
	}
	if tests[0].file.Kind != media.Still {
		t.Fatal("fixture classification broken")
	}
}

func TestCaptureTimeMissingFile(t *testing.T) {
	mtime := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	f := File{Rel: "x.dng", Abs: filepath.Join(t.TempDir(), "x.dng"), Kind: media.Still, Ext: "dng", ModTime: mtime}
	got, src := CaptureTime(f, pdt)
	if !got.Equal(mtime) || src != FromModTime {
		t.Errorf("got %v (%v)", got, src)
	}
}
