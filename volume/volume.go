// Package volume identifies the disk behind a path and finds where a disk
// is mounted now.
//
// Locations are pinned to volume UUIDs because mount points move: macOS
// mounts a second "Fast" as "/Volumes/Fast 1". Identify learns a volume's
// UUID from a path in it; Find turns a UUID back into today's mount point.
package volume

import (
	"bufio"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

// Info describes a mounted volume.
type Info struct {
	UUID       string
	Label      string
	MountPoint string
}

var (
	// ErrNotMounted means the volume exists in no current mount table.
	ErrNotMounted = errors.New("volume: not mounted")
	// ErrUnsupported means this platform has no implementation.
	ErrUnsupported = errors.New("volume: unsupported platform")
)

// plistStrings pulls the top-level <key>/<string> pairs out of an Apple XML
// plist, which is all `diskutil info -plist` needs to yield.
func plistStrings(r io.Reader) (map[string]string, error) {
	dec := xml.NewDecoder(r)
	out := map[string]string{}
	var key string
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			// plist > dict > (key|value): pairs live at depth 3.
			if depth != 3 {
				if t.Name.Local == "key" || t.Name.Local == "string" {
					dec.Skip()
					depth--
				}
				continue
			}
			var s string
			switch t.Name.Local {
			case "key":
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil, err
				}
				key = s
				depth--
			case "string":
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil, err
				}
				if key != "" {
					out[key] = s
				}
				key = ""
				depth--
			}
		case xml.EndElement:
			depth--
		}
	}
}

// parseFindmnt reads `findmnt -rn -o UUID,LABEL,TARGET` output: one line,
// space-separated, with octal escapes for awkward characters.
func parseFindmnt(r io.Reader) (Info, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		return Info{
			UUID:       unescape(fields[0]),
			Label:      unescape(fields[1]),
			MountPoint: unescape(fields[len(fields)-1]),
		}, nil
	}
	if err := sc.Err(); err != nil {
		return Info{}, err
	}
	return Info{}, ErrNotMounted
}

// unescape undoes findmnt's \040-style escapes.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
