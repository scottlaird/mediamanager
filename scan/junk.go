package scan

import "strings"

// junkNames are platform droppings that never count as content anywhere:
// not on sources, not in spools, and not as a "real file" in the link tree.
var junkNames = map[string]bool{
	".DS_Store":                 true,
	".Spotlight-V100":           true,
	".fseventsd":                true,
	".Trashes":                  true,
	".TemporaryItems":           true,
	".VolumeIcon.icns":          true,
	"Thumbs.db":                 true,
	"desktop.ini":               true,
	"$RECYCLE.BIN":              true,
	"System Volume Information": true,
}

// IsJunk reports whether a file or directory basename is a platform
// artefact to ignore. AppleDouble files (._name) and mediamanager's own
// .partial files count as junk too.
func IsJunk(name string) bool {
	if junkNames[name] {
		return true
	}
	if strings.HasPrefix(name, "._") {
		return true
	}
	return strings.HasSuffix(name, ".partial")
}
