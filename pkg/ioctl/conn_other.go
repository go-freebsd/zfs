//go:build !freebsd

package ioctl

// DevZFS is the control device of the ZFS kernel module.
const DevZFS = "/dev/zfs"

// Open always fails off FreeBSD.
func Open() (*Conn, error) { return nil, ErrUnsupported }

// OpenPath always fails off FreeBSD.
func OpenPath(path string) (*Conn, error) { return nil, ErrUnsupported }

// noPreempt does nothing off FreeBSD.
func noPreempt() func() { return func() {} }
