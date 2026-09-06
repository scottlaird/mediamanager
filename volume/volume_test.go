package volume

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

const samplePlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>APFSContainerReference</key>
	<string>disk3</string>
	<key>Bootable</key>
	<true/>
	<key>MountPoint</key>
	<string>/Volumes/Fast 1</string>
	<key>Nested</key>
	<dict>
		<key>VolumeUUID</key>
		<string>NOT-THIS-ONE</string>
	</dict>
	<key>Tags</key>
	<array>
		<string>ignored</string>
	</array>
	<key>VolumeName</key>
	<string>Fast</string>
	<key>VolumeUUID</key>
	<string>1234ABCD-0000-4000-8000-000000000000</string>
</dict>
</plist>
`

func TestPlistStrings(t *testing.T) {
	kv, err := plistStrings(strings.NewReader(samplePlist))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"APFSContainerReference": "disk3",
		"MountPoint":             "/Volumes/Fast 1",
		"VolumeName":             "Fast",
		"VolumeUUID":             "1234ABCD-0000-4000-8000-000000000000",
	}
	for k, v := range want {
		if kv[k] != v {
			t.Errorf("%s = %q, want %q", k, kv[k], v)
		}
	}
	if _, ok := kv["Bootable"]; ok {
		t.Error("non-string value captured")
	}
	if len(kv) != len(want) {
		t.Errorf("got %d keys %v, want %d", len(kv), kv, len(want))
	}
}

func TestParseFindmnt(t *testing.T) {
	tests := []struct {
		in   string
		want Info
		err  error
	}{
		{"1234-ABCD FAST /mnt/fast\n", Info{"1234-ABCD", "FAST", "/mnt/fast"}, nil},
		{`abcd Fast\x20Disk /media/scott/Fast\040Disk` + "\n", Info{"abcd", `Fast\x20Disk`, "/media/scott/Fast Disk"}, nil},
		{"\n", Info{}, ErrNotMounted},
		{"", Info{}, ErrNotMounted},
	}
	for _, tt := range tests {
		got, err := parseFindmnt(strings.NewReader(tt.in))
		if !errors.Is(err, tt.err) || got != tt.want {
			t.Errorf("parseFindmnt(%q) = %+v, %v; want %+v, %v", tt.in, got, err, tt.want, tt.err)
		}
	}
}

func TestIdentifyRealVolume(t *testing.T) {
	tool := map[string]string{"darwin": "diskutil", "linux": "findmnt"}[runtime.GOOS]
	if tool == "" {
		t.Skip("no implementation on", runtime.GOOS)
	}
	if _, err := exec.LookPath(tool); err != nil {
		t.Skipf("%s not installed", tool)
	}
	info, err := Identify(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if info.MountPoint == "" {
		t.Errorf("empty mount point: %+v", info)
	}
	if info.UUID == "" {
		t.Logf("volume has no UUID (tmpfs?): %+v", info)
		return
	}
	mount, err := Find(info.UUID)
	if err != nil {
		t.Fatalf("Find(%s): %v", info.UUID, err)
	}
	if mount != info.MountPoint {
		t.Errorf("Find = %q, Identify said %q", mount, info.MountPoint)
	}
	if _, err := Find("00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotMounted) {
		t.Errorf("unknown uuid: %v", err)
	}
}
