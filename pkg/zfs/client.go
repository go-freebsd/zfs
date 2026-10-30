package zfs

import (
	"context"
	"iter"

	"github.com/go-freebsd/zfs/pkg/primitives"
)

// Client is an open /dev/zfs. It is safe for concurrent use. Datasets and
// snapshots are reached through handles made by Dataset and Snapshot.
type Client struct {
	b   backend
	raw *primitives.Client // nil when built on a test backend
}

// Open opens /dev/zfs. The ZFS kernel module must be loaded and the caller
// needs the right to use it (usually root).
func Open() (*Client, error) {
	p, err := primitives.Open()
	if err != nil {
		return nil, err
	}
	return &Client{b: p, raw: p}, nil
}

// New wraps a primitives client. Close closes it.
func New(p *primitives.Client) *Client { return &Client{b: p, raw: p} }

// Primitives returns the low-level client for what this package does not
// cover. It is nil only for clients built by tests.
func (c *Client) Primitives() *primitives.Client { return c.raw }

// Close closes /dev/zfs. Handles made by the client stop working.
func (c *Client) Close() error { return c.b.Close() }

// Pools returns the imported pools as dataset handles.
func (c *Client) Pools() ([]*Dataset, error) {
	names, err := c.b.Pools()
	if err != nil {
		return nil, wrap("list pools", "", err)
	}
	out := make([]*Dataset, len(names))
	for i, n := range names {
		out[i] = c.Dataset(n)
	}
	return out, nil
}

// Dataset returns the handle of a filesystem or volume by its full name,
// "pool/path/to/dataset". It does not touch the kernel; use Exists or Info
// to find out whether the dataset is there.
func (c *Client) Dataset(name string) *Dataset { return &Dataset{c: c, name: name} }

// Snapshot returns the handle of a snapshot by its full name,
// "pool/path@snap".
func (c *Client) Snapshot(name string) *Snapshot { return &Snapshot{c: c, name: name} }

// Bookmark returns the handle of a bookmark by its full name,
// "pool/path#bookmark".
func (c *Client) Bookmark(name string) *Bookmark { return &Bookmark{c: c, name: name} }

// walk yields what primitives.Client.Walk visits. It stops when the caller
// breaks out of the loop or the context ends.
func (c *Client) walk(ctx context.Context, op, root string, opts primitives.ListOptions, skipRoot bool) iter.Seq2[*Info, error] {
	return func(yield func(*Info, error) bool) {
		stopped := false
		err := c.b.Walk(root, opts, func(d *primitives.Dataset) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if skipRoot && d.Name == root {
				return nil
			}
			if !yield(newInfo(c, d), nil) {
				stopped = true
				return errStop
			}
			return nil
		})
		if err != nil && !stopped {
			yield(nil, wrap(op, root, err))
		}
	}
}

type stopError struct{}

func (stopError) Error() string { return "stop" }

var errStop error = stopError{}
