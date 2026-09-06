//go:build !darwin && !linux

package volume

func Identify(path string) (Info, error) { return Info{}, ErrUnsupported }
func Find(uuid string) (string, error)   { return "", ErrUnsupported }
