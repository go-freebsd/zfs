//go:build freebsd

package ioctl

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// DevZFS is the control device of the ZFS kernel module.
const DevZFS = "/dev/zfs"

// Open opens /dev/zfs. The kernel module must be loaded.
func Open() (*Conn, error) { return OpenPath(DevZFS) }

// OpenPath opens the ZFS control device at path.
func OpenPath(path string) (*Conn, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	c := newConn(sysIoctl(fd))
	c.fd = fd
	c.closer = syscall.Close
	return c, nil
}

func sysIoctl(fd int) ioctlFunc {
	return func(ioc IOC, cmd *Cmd, _ *buffers) error {
		parm := iocparm{
			Version: iocVersion,
			Cmd:     uint64(uintptr(unsafe.Pointer(cmd))),
			CmdSize: uint64(unsafe.Sizeof(*cmd)),
		}
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), iocNumber(ioc), uintptr(unsafe.Pointer(&parm)))
		runtime.KeepAlive(cmd)
		if e != 0 {
			return e
		}
		return nil
	}
}
