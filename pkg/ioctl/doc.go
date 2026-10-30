// Package ioctl is the low-level interface to the ZFS kernel module: the
// zfs_cmd_t structure, the ZFS_IOC_* numbers and a connection to /dev/zfs
// that exchanges packed nvlists, without libzfs or cgo.
//
// Most programs want package zfs. Use this package to reach an ioctl that
// zfs does not wrap yet: build the input with github.com/go-freebsd/nvlist, call
// Conn.Call, and read the output list. Legacy ioctls that use fields of
// Cmd directly go through Conn.Do.
package ioctl
