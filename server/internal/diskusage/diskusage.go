// Package diskusage answers the one question the status page asks of the
// filesystem: how big is the volume this directory sits on, and how much of it
// is left.
//
// It is deliberately not a disk-usage *walker*. Nothing here adds up file
// sizes; it reads what the kernel already knows about the volume, which is the
// only figure that includes the bytes photod did not put there.
package diskusage

import "errors"

// ErrUnsupported is returned on a platform with no statfs. photod is a Linux
// daemon, so this exists to keep the package compiling on a developer's laptop
// rather than to promise anything.
var ErrUnsupported = errors.New("diskusage: not supported on this platform")

// Volume is one filesystem, as df would report it.
//
// Used and Free sum to Total by construction: Free is what an unprivileged
// writer may actually consume, so the blocks ext4 reserves for root are counted
// as used. They are not available to this archive, and a donut whose slices did
// not close would be a worse lie than calling five percent of the drive "in use
// by something else".
type Volume struct {
	Path  string `json:"path"`
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
	Free  uint64 `json:"free"`
	// Device identifies the volume itself, so two paths can be told apart
	// without comparing mount points. The archive and the derivatives are
	// normally on different disks — see DERIVATIVES_ROOT — and the status page
	// has to know whether it is drawing one pie or two.
	Device uint64 `json:"-"`
}

// SameVolume reports whether two directories live on one filesystem.
func SameVolume(a, b Volume) bool { return a.Device == b.Device && a.Device != 0 }
