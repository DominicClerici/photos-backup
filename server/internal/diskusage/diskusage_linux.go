//go:build linux

package diskusage

import (
	"fmt"
	"os"
	"syscall"
)

// Stat reports the volume a directory sits on.
func Stat(path string) (Volume, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return Volume{}, fmt.Errorf("statfs %s: %w", path, err)
	}

	block := uint64(fs.Bsize)
	v := Volume{
		Path:  path,
		Total: fs.Blocks * block,
		Free:  fs.Bavail * block,
	}
	v.Used = v.Total - v.Free

	// The device number, not the mount point: a bind mount would give two names
	// to one disk, and adding its bytes up twice is exactly the mistake this is
	// here to prevent.
	if info, err := os.Stat(path); err == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			v.Device = uint64(st.Dev)
		}
	}
	return v, nil
}
