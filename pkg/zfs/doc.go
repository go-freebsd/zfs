// Package zfs is a ZFS client that talks to the kernel through /dev/zfs
// ioctls, without libzfs or cgo.
//
// This version can read: list datasets, snapshots and bookmarks, get their
// properties, holds and space accounting, and list the imported pools.
//
// Property values are returned as the kernel stores them: numbers and
// strings, each with the dataset it was set on. Enumerated properties
// (compression, checksum, ...) are therefore numbers, not names.
//
// Errors from the kernel are *ioctl.Error values that unwrap to the errno, so
// errors.Is(err, fs.ErrNotExist) and friends work.
package zfs
