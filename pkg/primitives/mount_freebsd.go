//go:build freebsd

package primitives

import (
	"io"
	"os"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

const (
	sysNmount = 378     // SYS_nmount
	mntForce  = 0x80000 // MNT_FORCE
	errmsgLen = 256
)

// osMounter mounts with nmount(2), as libzfs does on FreeBSD.
type osMounter struct{}

func iov(b []byte) syscall.Iovec {
	v := syscall.Iovec{}
	if len(b) > 0 {
		v.Base = &b[0]
	}
	v.SetLen(len(b))
	return v
}

func cstr(s string) []byte { return append([]byte(s), 0) }

// nmount calls nmount(2) with fstype, fspath and from, then one boolean
// option per entry of opts (an entry may be "a/b", which libzfs also splits).
func (osMounter) nmount(dataset, path string, opts []string) error {
	var keep [][]byte
	var iovs []syscall.Iovec
	add := func(name string, val []byte) {
		n := cstr(name)
		keep = append(keep, n, val)
		iovs = append(iovs, iov(n), iov(val))
	}
	var upd bool
	var rest []string
	for _, o := range opts {
		for _, p := range strings.FieldsFunc(o, func(r rune) bool { return r == ',' || r == '/' }) {
			if p == "update" {
				upd = true
			} else {
				rest = append(rest, p)
			}
		}
	}
	if upd {
		add("update", nil)
	}
	add("fstype", cstr("zfs"))
	add("fspath", cstr(path))
	add("from", cstr(dataset))
	for _, o := range rest {
		add(o, nil)
	}
	errmsg := make([]byte, errmsgLen)
	add("errmsg", errmsg)

	_, _, e := syscall.Syscall(sysNmount, uintptr(unsafe.Pointer(&iovs[0])), uintptr(len(iovs)), 0)
	runtime.KeepAlive(keep)
	runtime.KeepAlive(iovs)
	if e != 0 {
		if n := cstrLen(errmsg); n > 0 {
			return &kernelError{errno: e, msg: string(errmsg[:n])}
		}
		return e
	}
	return nil
}

func cstrLen(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return len(b)
}

// kernelError carries the message nmount puts in its errmsg buffer.
type kernelError struct {
	errno syscall.Errno
	msg   string
}

func (e *kernelError) Error() string { return e.errno.Error() + ": " + e.msg }
func (e *kernelError) Unwrap() error { return e.errno }

func (osMounter) unmount(path string, force bool) error {
	flags := 0
	if force {
		flags = mntForce
	}
	return syscall.Unmount(path, flags)
}

func (osMounter) mounts() ([]Mount, error) {
	n, err := syscall.Getfsstat(nil, 2) // MNT_NOWAIT
	if err != nil {
		return nil, err
	}
	for {
		buf := make([]syscall.Statfs_t, n+8)
		got, err := syscall.Getfsstat(buf, 2)
		if err != nil {
			return nil, err
		}
		if got > len(buf) {
			n = got
			continue
		}
		var out []Mount
		for i := 0; i < got; i++ {
			s := &buf[i]
			if cs(s.Fstypename[:]) != "zfs" {
				continue
			}
			out = append(out, Mount{Dataset: cs(s.Mntfromname[:]), Mountpoint: cs(s.Mntonname[:])})
		}
		return out, nil
	}
}

func cs[T ~int8 | ~uint8](b []T) string {
	var out []byte
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

func (osMounter) mkdirAll(path string) error { return os.MkdirAll(path, 0o755) }

func (osMounter) isEmptyDir(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err
}

func (osMounter) rmdir(path string) error { return syscall.Rmdir(path) }

// jailed reports whether the process runs inside a jail.
func (osMounter) jailed() bool {
	v, err := syscall.Sysctl("security.jail.jailed")
	return err == nil && v != "" && v[0] == 1
}
