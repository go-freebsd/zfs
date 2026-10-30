package primitives

import (
	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// caller is the part of *ioctl.Conn the client uses; tests replace it with
// a fake kernel.
type caller interface {
	Do(ioc ioctl.IOC, cmd *ioctl.Cmd, in *nvlist.List) (*nvlist.List, error)
	Call(ioc ioctl.IOC, name string, in *nvlist.List) (*nvlist.List, error)
	Ioctl(ioc ioctl.IOC, cmd *ioctl.Cmd) error
}

// Client reads ZFS state. It is safe for concurrent use.
type Client struct {
	c    caller
	conn *ioctl.Conn // nil for fakes
}

// Open opens /dev/zfs. The ZFS kernel module must be loaded.
func Open() (*Client, error) {
	conn, err := ioctl.Open()
	if err != nil {
		return nil, err
	}
	return &Client{c: conn, conn: conn}, nil
}

// NewClient returns a client on an existing connection. The client does not
// own it: Close does not close conn.
func NewClient(conn *ioctl.Conn) *Client { return &Client{c: conn} }

func newClient(c caller) *Client { return &Client{c: c} }

// Close closes the connection opened by Open.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
