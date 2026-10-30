// Package primitives is the low-level ZFS client: one method per kernel
// operation, taking names and natural Go values, over /dev/zfs ioctls and
// without libzfs or cgo. Most programs want the handle based API in package
// zfs, which is built on this one; use this package for raw properties or
// when the exact kernel call matters.
//
// It can list datasets, snapshots and bookmarks, read and set properties,
// create, clone, rename, roll back, promote and destroy, manage holds and
// bookmarks, and load and change encryption keys.
//
// Dataset.Props holds property values as the kernel stores them: numbers and
// strings, each with the dataset it was set on. Enumerated properties
// (compression, checksum, ...) are numbers there; Dataset.Enum, Bool, Time,
// Ratio and Value give them in natural Go types, using the table that
// LookupProp exposes. SetProps takes the same natural types.
//
// Errors from the kernel are *ioctl.Error values that unwrap to the errno, so
// errors.Is(err, fs.ErrNotExist) and friends work.
package primitives
