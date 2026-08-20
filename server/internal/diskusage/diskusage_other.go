//go:build !linux

package diskusage

func Stat(path string) (Volume, error) { return Volume{Path: path}, ErrUnsupported }
