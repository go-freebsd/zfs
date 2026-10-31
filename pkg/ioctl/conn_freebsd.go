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

const (
	sigURG      = 16 // the signal Go uses to preempt a goroutine
	sigBlock    = 1  // SIG_BLOCK
	sigSetMask  = 3  // SIG_SETMASK
	sigsetWords = 4  // _SIG_WORDS
)

// noPreempt blocks SIGURG on the calling thread, which stays the goroutine's
// thread until the returned function restores the mask. See Conn.DoOnce.
func noPreempt() func() {
	runtime.LockOSThread()
	var set, old [sigsetWords]uint32
	set[(sigURG-1)/32] = 1 << ((sigURG - 1) % 32)
	if _, _, e := syscall.RawSyscall(syscall.SYS_SIGPROCMASK, sigBlock, uintptr(unsafe.Pointer(&set[0])), uintptr(unsafe.Pointer(&old[0]))); e != 0 {
		runtime.UnlockOSThread()
		return func() {}
	}
	return func() {
		syscall.RawSyscall(syscall.SYS_SIGPROCMASK, sigSetMask, uintptr(unsafe.Pointer(&old[0])), 0)
		runtime.UnlockOSThread()
	}
}

// threadSignalBlocked reports whether SIGURG is blocked on the calling thread.
func threadSignalBlocked() bool {
	var cur [sigsetWords]uint32
	syscall.RawSyscall(syscall.SYS_SIGPROCMASK, sigBlock, 0, uintptr(unsafe.Pointer(&cur[0])))
	return cur[(sigURG-1)/32]&(1<<((sigURG-1)%32)) != 0
}
