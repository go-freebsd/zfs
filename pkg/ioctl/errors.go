package ioctl

import (
	"errors"
	"fmt"
	"strconv"
	"syscall"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
)

// ErrUnsupported is returned on platforms other than FreeBSD.
var ErrUnsupported = errors.New("zfs: only supported on FreeBSD")

func itoa(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

// Error is a failed ZFS ioctl. It unwraps to the errno, so
// errors.Is(err, fs.ErrNotExist) and errors.Is(err, syscall.EEXIST) work.
type Error struct {
	Ioc   IOC
	Name  string // zc_name: the pool or dataset the call was about
	Errno syscall.Errno
	// Detail is the list the kernel returned together with the error, if any.
	// For calls that act on several items (set properties, snapshots, holds)
	// it maps each failed item to its own errno (an int32 pair).
	Detail *nvlist.List
}

func (e *Error) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("%v %q: %v", e.Ioc, e.Name, e.Errno)
	}
	return fmt.Sprintf("%v: %v", e.Ioc, e.Errno)
}

func (e *Error) Unwrap() error { return e.Errno }
