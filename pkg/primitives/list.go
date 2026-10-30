package primitives

import (
	"errors"
	"sort"
	"syscall"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// Unlimited as ListOptions.Depth descends through every child.
const Unlimited = -1

// ListOptions selects what List and Walk return.
type ListOptions struct {
	// Types selects the kinds to return; the default is Filesystem|Volume.
	// Snapshots and bookmarks are listed for every dataset visited.
	Types Type
	// Depth is how many levels below the root to visit: 0 is the root only,
	// 1 adds its children, Unlimited descends through everything.
	Depth int
	// Simple skips the property lists (much cheaper); datasets then carry
	// only the fields of Dataset that are not properties.
	Simple bool
	// MinTxg and MaxTxg restrict snapshots to a creation transaction range
	// (0 means no limit).
	MinTxg, MaxTxg uint64
}

func (o ListOptions) types() Type {
	if o.Types == 0 {
		return Filesystem | Volume
	}
	return o.Types
}

// List returns root and what is below it according to opts, in the order of
// "zfs list -r": a dataset, then its snapshots (by creation), its bookmarks,
// then its children sorted by name, each followed by its own contents.
func (c *Client) List(root string, opts ListOptions) ([]*Dataset, error) {
	var out []*Dataset
	err := c.Walk(root, opts, func(d *Dataset) error {
		out = append(out, d)
		return nil
	})
	return out, err
}

// Walk calls fn for every dataset List would return. It stops at the first
// error from fn and returns it.
func (c *Client) Walk(root string, opts ListOptions, fn func(*Dataset) error) error {
	// A missing root is an error, not an empty listing.
	d, err := c.get(root, opts.Simple, nil)
	if err != nil {
		return err
	}
	return c.walk(d, opts, 0, fn)
}

func (c *Client) walk(d *Dataset, opts ListOptions, depth int, fn func(*Dataset) error) error {
	want := opts.types()
	if d.Type&want != 0 {
		if err := fn(d); err != nil {
			return err
		}
	}
	if d.Type == Snapshot || d.Type == Bookmark {
		return nil
	}

	if want&Snapshot != 0 {
		var snaps []*Dataset
		if err := c.iterate(ioctl.IOCSnapshotListNext, d.Name, opts, func(s *Dataset) error {
			snaps = append(snaps, s)
			return nil
		}); err != nil {
			return err
		}
		sort.SliceStable(snaps, func(i, j int) bool {
			if snaps[i].CreationTxg != snaps[j].CreationTxg {
				return snaps[i].CreationTxg < snaps[j].CreationTxg
			}
			return snaps[i].Name < snaps[j].Name
		})
		for _, s := range snaps {
			if err := fn(s); err != nil {
				return err
			}
		}
	}
	if want&Bookmark != 0 {
		bms, err := c.Bookmarks(d.Name)
		if err != nil {
			return err
		}
		for _, b := range bms {
			if err := fn(b); err != nil {
				return err
			}
		}
	}

	if opts.Depth != Unlimited && depth >= opts.Depth {
		return nil
	}
	// The kernel returns children in ZAP order; the entries already carry
	// their stats and properties, so no further ioctl is needed per child.
	var kids []*Dataset
	if err := c.iterate(ioctl.IOCDatasetListNext, d.Name, opts, func(k *Dataset) error {
		kids = append(kids, k)
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(kids, func(i, j int) bool { return kids[i].Name < kids[j].Name })
	for _, k := range kids {
		if err := c.walk(k, opts, depth+1, fn); err != nil {
			return err
		}
	}
	return nil
}

// iterate runs a *_LIST_NEXT ioctl until the kernel reports the end of the
// list. One Cmd is reused so the cursor (zc_cookie, zc_obj) persists, but
// zc_name is set to the parent before every call because the kernel
// overwrites it with the child's name.
func (c *Client) iterate(ioc ioctl.IOC, parent string, opts ListOptions, fn func(*Dataset) error) error {
	var in *nvlist.List
	if ioc == ioctl.IOCSnapshotListNext && (opts.MinTxg != 0 || opts.MaxTxg != 0) {
		in = nvlist.New()
		if opts.MinTxg != 0 {
			in.Add("snap_iter_min_txg", opts.MinTxg)
		}
		if opts.MaxTxg != 0 {
			in.Add("snap_iter_max_txg", opts.MaxTxg)
		}
	}
	cmd := ioctl.NewCmd()
	if opts.Simple {
		cmd.Simple = 1
	}
	for {
		if err := cmd.SetName(parent); err != nil {
			return err
		}
		cmd.ObjsetStats.CreationTxg = 0
		out, err := c.c.Do(ioc, cmd, in)
		if err != nil {
			// ESRCH is the normal end; ENOENT means the parent was
			// destroyed while we were listing it.
			if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.ENOENT) {
				return nil
			}
			return err
		}
		d, err := datasetFrom(cmd, out)
		if err != nil {
			return err
		}
		if err := fn(d); err != nil {
			return err
		}
	}
}
