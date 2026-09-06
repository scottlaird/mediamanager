package volume

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
)

// Identify returns the volume containing path, via findmnt.
func Identify(path string) (Info, error) {
	out, err := findmnt("-T", path)
	if err != nil {
		return Info{}, err
	}
	return parseFindmnt(bytes.NewReader(out))
}

// Find returns where the volume with this UUID is mounted, via findmnt.
func Find(uuid string) (string, error) {
	out, err := findmnt("-S", "UUID="+uuid)
	if err != nil {
		return "", err
	}
	info, err := parseFindmnt(bytes.NewReader(out))
	if err != nil {
		return "", err
	}
	return info.MountPoint, nil
}

func findmnt(args ...string) ([]byte, error) {
	args = append([]string{"-rn", "-o", "UUID,LABEL,TARGET"}, args...)
	out, err := exec.Command("findmnt", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, ErrNotMounted
		}
		return nil, fmt.Errorf("volume: findmnt: %w", err)
	}
	return out, nil
}
