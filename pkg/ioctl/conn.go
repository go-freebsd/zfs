package ioctl

import (
	"errors"
	"runtime"
	"sync"
	"syscall"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
)

const minDstSize = 128 << 10 // libzfs_core starts at max(2*src, 128 KiB)

// maxDstSize bounds the destination buffer growth. It is a variable so the
// tests can lower it.
var maxDstSize = 1 << 30

// buffers are the nvlist buffers of one call. Cmd only holds their
// addresses, so they are handed to the platform layer to keep alive (and,
// in tests, to read and write).
type buffers struct {
	src, dst []byte
}

// ioctlFunc performs one ioctl on /dev/zfs.
type ioctlFunc func(ioc IOC, cmd *Cmd, b *buffers) error

// Conn is an open /dev/zfs. It is safe for concurrent use.
type Conn struct {
	fd     int
	do     ioctlFunc
	closer func(fd int) error

	// Trace, if set, receives the packed source and destination nvlists of
	// every successful Do/Call. It is meant for debugging and for capturing
	// test vectors; set it before sharing the Conn between goroutines.
	Trace func(ioc IOC, name string, src, dst []byte)

	mu     sync.Mutex
	closed bool
}

func newConn(do ioctlFunc) *Conn { return &Conn{fd: -1, do: do} }

// Close closes the connection.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.closer != nil {
		return c.closer(c.fd)
	}
	return nil
}

// FD returns the descriptor of /dev/zfs, for ioctls that need it (for
// example as the cleanup_fd of a hold).
func (c *Conn) FD() int { return c.fd }

// Do runs one ioctl with cmd. in, if not nil, is packed into the source
// nvlist. The destination buffer starts at 128 KiB and doubles whenever the
// kernel reports ENOMEM or a larger required size, as libzfs_core does. The
// output list is returned if the kernel produced one.
//
// cmd must come from NewCmd. Its Name must be set by the caller; Do sets
// the Nvlist* fields.
func (c *Conn) Do(ioc IOC, cmd *Cmd, in *nvlist.List) (*nvlist.List, error) {
	var src []byte
	if in != nil {
		var err error
		if src, err = nvlist.Pack(in); err != nil {
			return nil, err
		}
	}
	cmd.NvlistSrc, cmd.NvlistSrcSize = ptr(src), uint64(len(src))

	size := max(2*len(src), minDstSize)
	for {
		dst := make([]byte, size)
		cmd.NvlistDst, cmd.NvlistDstSize, cmd.NvlistDstFilled = ptr(dst), uint64(size), 0

		err := c.do(ioc, cmd, &buffers{src: src, dst: dst})
		runtime.KeepAlive(src)
		runtime.KeepAlive(dst)

		// On FreeBSD a too small buffer is not an error: the kernel returns
		// success with the size it needs in zc_nvlist_dst_size.
		grew := err == nil && cmd.NvlistDstSize > uint64(size)
		if grew || errors.Is(err, syscall.ENOMEM) {
			next := size * 2
			if grew && cmd.NvlistDstSize > uint64(next) && cmd.NvlistDstSize <= uint64(maxDstSize) {
				next = int(cmd.NvlistDstSize)
			}
			if next > maxDstSize {
				return nil, &Error{Ioc: ioc, Name: cmd.GetName(), Errno: syscall.ENOMEM}
			}
			size = next
			continue
		}
		if err != nil {
			return nil, c.wrap(ioc, cmd, err)
		}
		if cmd.NvlistDstFilled == 0 || cmd.NvlistDstSize == 0 {
			if c.Trace != nil {
				c.Trace(ioc, cmd.GetName(), src, nil)
			}
			return nil, nil
		}
		out := dst[:cmd.NvlistDstSize]
		if c.Trace != nil {
			c.Trace(ioc, cmd.GetName(), src, out)
		}
		return nvlist.Unpack(out)
	}
}

// Call runs ioctl ioc on the named pool or dataset with the given input
// list and returns the output list (nil if the ioctl produced none).
func (c *Conn) Call(ioc IOC, name string, in *nvlist.List) (*nvlist.List, error) {
	cmd := NewCmd()
	if err := cmd.SetName(name); err != nil {
		return nil, err
	}
	return c.Do(ioc, cmd, in)
}

// Ioctl runs an ioctl that uses only the legacy fields of cmd, with no
// nvlist buffers.
func (c *Conn) Ioctl(ioc IOC, cmd *Cmd) error {
	cmd.NvlistSrc, cmd.NvlistSrcSize, cmd.NvlistDst, cmd.NvlistDstSize = 0, 0, 0, 0
	if err := c.do(ioc, cmd, &buffers{}); err != nil {
		return c.wrap(ioc, cmd, err)
	}
	return nil
}

func (c *Conn) wrap(ioc IOC, cmd *Cmd, err error) error {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return &Error{Ioc: ioc, Name: cmd.GetName(), Errno: errno}
	}
	return err
}
