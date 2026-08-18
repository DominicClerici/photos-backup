package verify

import (
	"errors"
	"os"
	"syscall"
)

// isCrossDevice reports whether a link failed because the two paths are on
// different filesystems, which is the one link failure with a useful answer:
// re-run with --copy.
func isCrossDevice(err *os.LinkError) bool {
	return errors.Is(err.Err, syscall.EXDEV)
}
