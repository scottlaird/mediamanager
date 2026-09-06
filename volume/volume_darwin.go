package volume

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// Identify returns the volume containing path. The mount point comes from
// statfs; the UUID and name from diskutil.
func Identify(path string) (Info, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Info{}, fmt.Errorf("volume: statfs %s: %w", path, err)
	}
	mount := cstring(st.Mntonname[:])
	kv, err := diskutil(mount)
	if err != nil {
		return Info{}, err
	}
	return Info{UUID: kv["VolumeUUID"], Label: kv["VolumeName"], MountPoint: mount}, nil
}

// Find returns where the volume with this UUID is mounted.
func Find(uuid string) (string, error) {
	kv, err := diskutil(uuid)
	if err != nil {
		return "", err
	}
	if kv["MountPoint"] == "" {
		return "", ErrNotMounted
	}
	return kv["MountPoint"], nil
}

func diskutil(arg string) (map[string]string, error) {
	out, err := exec.Command("diskutil", "info", "-plist", arg).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("volume: diskutil info %s: %w", arg, ErrNotMounted)
		}
		return nil, fmt.Errorf("volume: diskutil: %w", err)
	}
	return plistStrings(bytes.NewReader(out))
}

func cstring(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}
