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
		return fmt.Sprintf("%v %q: %v", e.Ioc, e.Name, ErrnoString(e.Errno))
	}
	return fmt.Sprintf("%v: %v", e.Ioc, ErrnoString(e.Errno))
}

// Errors only ZFS returns: ZFS_ERR_* from <sys/fs/zfs.h>, numbered from
// 1024. They are syscall.Errno values, so errors.Is(err, ioctl.ErrStreamTruncated)
// works on the error of a failed call.
const (
	ErrCheckpointExists          syscall.Errno = 1024 + iota // a checkpoint exists
	ErrDiscardingCheckpoint                                  // a checkpoint is being discarded
	ErrNoCheckpoint                                          // there is no checkpoint
	ErrDevRemovalInProgress                                  // a device removal is in progress
	ErrVdevTooBig                                            // the device is too big
	ErrIocCmdUnavail                                         // the ioctl is not available
	ErrIocArgUnavail                                         // an argument is not available
	ErrIocArgRequired                                        // an argument is required
	ErrIocArgBadType                                         // an argument has the wrong type
	ErrWrongParent                                           // the parent is wrong
	ErrFromIvsetGUIDMissing                                  // the incremental source has no ivset guid
	ErrFromIvsetGUIDMismatch                                 // the ivset guid of the incremental source differs
	ErrSpillBlockFlagMissing                                 // spill block flag missing (raw stream from an old version)
	ErrUnknownSendStreamFeature                              // the stream uses a feature this system does not know
	ErrExportInProgress                                      // the pool is being exported
	ErrBookmarkSourceNotAncestor                             // the bookmark source is not an ancestor
	ErrStreamTruncated                                       // the send stream ends too early
	ErrStreamLargeBlockMismatch                              // the stream has large blocks but the dataset cannot take them
	ErrResilverInProgress                                    // a resilver is in progress
	ErrRebuildInProgress                                     // a rebuild is in progress
	ErrBadProp                                               // a bad property in the stream
	ErrVdevNotSupported                                      // the device is not supported
	ErrNotUserNamespace                                      // not in the user namespace
	ErrResumeExists                                          // a resumable receive already exists
	ErrCryptoNotSupported                                    // the encryption feature is not supported
	ErrRaidzExpandInProgress                                 // a raidz expansion is in progress
	ErrAshiftMismatch                                        // the ashift differs
)

var zfsErrnoText = [...]string{
	"a checkpoint exists", "a checkpoint is being discarded", "there is no checkpoint",
	"a device removal is in progress", "the device is too big", "the ioctl is not available",
	"an ioctl argument is not available", "an ioctl argument is required", "an ioctl argument has the wrong type",
	"wrong parent", "the incremental source has no ivset guid", "the ivset guid of the incremental source differs",
	"spill block flag missing", "the stream uses a feature this system does not support", "the pool is being exported",
	"the bookmark source is not an ancestor", "the stream is truncated", "large block setting of the stream does not match",
	"a resilver is in progress", "a rebuild is in progress", "bad property", "the device is not supported",
	"not in the user namespace", "a resumable receive already exists", "the encryption feature is not supported",
	"a raidz expansion is in progress", "the ashift differs",
}

// ErrnoString describes an errno, including the ZFS specific ones.
func ErrnoString(e syscall.Errno) string {
	if i := int(e) - 1024; i >= 0 && i < len(zfsErrnoText) {
		return zfsErrnoText[i]
	}
	return e.Error()
}

func (e *Error) Unwrap() error { return e.Errno }
