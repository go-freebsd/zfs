package primitives

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// receivePackage receives the streams that follow the compound header h of a
// replication stream, as zfs_receive_package does: each snapshot stream is
// received with the properties, snapshot properties and holds its dataset
// had on the sender, until the closing END record. It returns what was
// received, also when a stream failed.
func (c *Client) receivePackage(ctx context.Context, src *os.File, h *StreamHeader, target string, o *ReceiveOptions, prev *StreamHeader) ([]Received, error) {
	p, err := readPackage(src, h)
	if err != nil {
		return nil, ctxOr(ctx, err)
	}
	if p.Recursive() && strings.Contains(target, "@") {
		return nil, errors.New("zfs: receive: cannot specify a snapshot name for a multi-snapshot stream")
	}
	sendfs := h.Dataset()
	holds := h.Features&FeatureHolds != 0 && !o.SkipHolds

	// An incremental stream of a tree: first bring the target in line with
	// what the sender did to its tree since the base snapshot.
	var soft bool
	var tofs string
	reconciling := p.FromSnap != "" && p.Recursive() && !o.DryRun
	if reconciling {
		hn, err := receiveNameFrom(target, h, o.Layout, sendfs)
		if err != nil {
			return nil, err
		}
		tofs, _, _ = strings.Cut(hn, "@")
		var renamed []string
		soft = c.reconcile(p, tofs, o, &renamed)
		for _, n := range renamed { // do not receive into a tree that is mounted at its old place
			if cl, err := c.gather(n, "mountpoint", false); err == nil {
				cl.prefix()
			}
		}
	}

	var (
		got  []Received
		top  string
		last = prev
	)
	for {
		if err := ctx.Err(); err != nil {
			return got, err
		}
		sh, end, err := readHeaderOrEnd(src)
		if err != nil {
			if err == io.EOF {
				err = fmt.Errorf("%w: the replication stream ends without its closing record", ErrNotAStream)
			}
			return got, ctxOr(ctx, err)
		}
		if end {
			break
		}
		if sh.Compound {
			return got, fmt.Errorf("%w: a replication stream inside a replication stream", ErrNotAStream)
		}
		name, err := receiveNameFrom(target, sh, o.Layout, sendfs)
		if err != nil {
			return got, err
		}
		var rec Received
		if o.DryRun {
			rec, err = c.receiveDry(sh, last, src, name, o)
			last = sh
			got = append(got, rec)
			if err != nil {
				return got, ctxOr(ctx, err)
			}
			continue
		}
		fs, snapName, found := p.find(sh.ToGUID)
		sub := &substream{noMount: true}
		if found {
			if props, ok := fs.List("props"); ok {
				sub.props = withoutKeylocation(props)
			}
		}
		rec, err = c.receiveFD(sh, int(src.Fd()), name, o, sub)
		if err != nil && isExists(err) {
			// The snapshot is there already (an earlier, interrupted
			// transfer): read the stream past and go on.
			ds, _, _ := strings.Cut(name, "@")
			if ok, _ := c.hasGUID(ds, sh.ToGUID); ok {
				n, serr := skipStream(src, sh, name)
				rec.Bytes, rec.Skipped = n, true
				err = serr
			}
		}
		got = append(got, rec)
		if err != nil {
			return got, ctxOr(ctx, err)
		}
		if top == "" {
			top, _, _ = strings.Cut(name, "@")
		}
		if rec.Skipped {
			continue
		}
		if found {
			if sp, ok := fs.List("snapprops"); ok {
				if props, ok := sp.List(snapName); ok && props.Len() > 0 {
					c.setReceivedProps(name, props) // best effort, as libzfs
				}
			}
			if holds {
				if sh, ok := fs.List("snapholds"); ok {
					if tags, ok := sh.List(snapName); ok {
						for _, t := range tags.Pairs() {
							c.Hold(t.Name, name) // best effort, as libzfs
						}
					}
				}
			}
		}
	}
	if reconciling && c.reconcile(p, tofs, o, nil) {
		soft = true
	}
	var mountErr error
	if !o.DryRun && !o.NoMount && top != "" {
		mountErr = c.MountAll(top, nil)
	}
	if mountErr != nil {
		return got, mountErr
	}
	if soft {
		return got, ErrPartialReceive
	}
	return got, nil
}

// withoutKeylocation drops keylocation from a property list: it can only be
// set on an encryption root, which a received dataset may not be yet.
func withoutKeylocation(props *nvlist.List) *nvlist.List {
	if !props.Has("keylocation") {
		return props
	}
	return copyList(props, []string{"keylocation"})
}

func isExists(err error) bool {
	return errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOENT)
}

// hasGUID reports whether a snapshot of the dataset has the GUID.
func (c *Client) hasGUID(dataset string, guid uint64) (bool, error) {
	var found bool
	err := c.Walk(dataset, ListOptions{Types: Snapshot, Depth: 1}, func(d *Dataset) error {
		if d.GUID == guid {
			found = true
		}
		return nil
	})
	return found, err
}

// setReceivedProps sets properties on a snapshot as received values
// (ZFS_IOC_SET_PROP with zc_cookie set).
func (c *Client) setReceivedProps(snapshot string, props *nvlist.List) error {
	cmd := ioctl.NewCmd()
	if err := cmd.SetName(snapshot); err != nil {
		return err
	}
	cmd.Cookie = 1
	_, err := c.c.Do(ioctl.IOCSetProp, cmd, props)
	return err
}
