// Package zfs is a ZFS client that talks to the kernel through /dev/zfs
// ioctls, without libzfs or cgo.
//
// This version can read: list datasets, snapshots and bookmarks, get their
// properties, holds and space accounting, and list the imported pools. It can
// set and inherit properties.
//
// Dataset.Props holds property values as the kernel stores them: numbers and
// strings, each with the dataset it was set on. Enumerated properties
// (compression, checksum, ...) are numbers there; Dataset.Enum, Bool, Time,
// Ratio and Value give them in natural Go types, using the table that
// LookupProp exposes. SetProps takes the same natural types.
//
// Errors from the kernel are *ioctl.Error values that unwrap to the errno, so
// errors.Is(err, fs.ErrNotExist) and friends work.
package zfs
