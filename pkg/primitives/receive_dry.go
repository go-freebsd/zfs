package primitives

import (
	"io"
	"strings"
	"syscall"

	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// receiveDry is Receive with ReceiveOptions.DryRun: it makes the checks that
// zfs receive -n makes and reads the stream to its end without receiving it.
func (c *Client) receiveDry(h *StreamHeader, r io.Reader, snapshot string, o *ReceiveOptions) (Received, error) {
	rec := Received{Header: h, Snapshot: snapshot, DryRun: true}
	fail := func(err error) (Received, error) {
		return rec, &ReceiveError{Received: rec, Err: err}
	}
	if err := c.dryRunChecks(h, snapshot, o); err != nil {
		return fail(err)
	}
	n, err := skipStream(r, h, snapshot)
	rec.Bytes = n + ioctl.ReplayRecordSize
	if err != nil {
		return fail(err)
	}
	return rec, nil
}

// dryRunChecks are the checks libzfs makes in zfs_receive_one before it
// calls the kernel, and the property checks of the command line.
func (c *Client) dryRunChecks(h *StreamHeader, snapshot string, o *ReceiveOptions) error {
	ds, _, _ := strings.Cut(snapshot, "@")
	no := func(e syscall.Errno) error {
		return &ioctl.Error{Ioc: ioctl.IOCRecvNew, Name: ds, Errno: e}
	}
	exists, err := c.Exists(ds)
	if err != nil {
		return err
	}
	var target *Dataset
	if exists {
		if target, err = c.get(ds, false, nil); err != nil {
			return err
		}
	}
	if _, err := excludeProps(h, o, target); err != nil {
		return err
	}
	for _, k := range sortedKeys(o.Props) {
		if _, err := convertProp(k, o.Props[k], false); err != nil {
			return err
		}
	}

	fromOrigin := h.FromGUID == 0 || h.Clone || o.Origin != ""
	newfs := fromOrigin && !h.Resuming
	if !exists {
		i := strings.LastIndexByte(ds, '/')
		if !fromOrigin || i < 0 {
			return no(syscall.ENOENT) // an incremental stream needs its target
		}
		parent := ds[:i]
		ok, err := c.Exists(parent)
		if err != nil {
			return err
		}
		if !ok {
			if !o.Parents {
				return no(syscall.ENOENT)
			}
			return nil
		}
		p, err := c.get(parent, true, nil)
		if err != nil {
			return err
		}
		if p.Type != Filesystem {
			return no(syscall.EINVAL)
		}
		return nil
	}

	var snaps []*Dataset
	if err := c.Walk(ds, ListOptions{Types: Snapshot, Depth: 0, Simple: true}, func(d *Dataset) error {
		snaps = append(snaps, d)
		return nil
	}); err != nil {
		return err
	}
	enc, _ := target.Enum("encryption")
	encrypted := enc != "" && enc != "off"

	if newfs {
		if !o.Force || len(snaps) > 0 || target.Origin != "" {
			return no(syscall.EEXIST)
		}
		if h.Type == Volume && !strings.Contains(ds, "/") {
			return no(syscall.EEXIST)
		}
		if o.Force && (encrypted || h.Raw()) {
			return no(syscall.EINVAL)
		}
		return nil
	}
	if h.Resuming && fromOrigin && !target.Inconsistent && !o.Force {
		return no(syscall.EEXIST)
	}
	if !fromOrigin && !encrypted && h.Raw() {
		return no(syscall.EINVAL)
	}
	if !fromOrigin && !h.Resuming {
		// The base must be a snapshot of the target, and the latest one
		// unless the receive may roll back.
		var latest *Dataset
		found := false
		for _, s := range snaps {
			if latest == nil || s.CreationTxg > latest.CreationTxg {
				latest = s
			}
			if s.GUID == h.FromGUID {
				found = true
			}
		}
		if !found || (!o.Force && latest.GUID != h.FromGUID) {
			return no(syscall.ENODEV)
		}
	}
	return nil
}
