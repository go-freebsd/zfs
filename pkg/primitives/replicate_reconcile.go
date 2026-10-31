package primitives

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
)

// ErrPartialReceive is returned, after all the data was received, when the
// target could not be brought in line with the stream: a snapshot or dataset
// that the stream no longer has could not be destroyed, or one that the
// stream names differently could not be renamed or promoted (zfs receive
// exits with 2 in that case).
var ErrPartialReceive = errors.New("zfs: receive: some datasets could not be destroyed, renamed or promoted")

var recvSeq atomic.Uint64

// reconcile is recv_incremental_replication: before and after the streams of
// an incremental replication stream are received, the target tree is made to
// look like the sender's: snapshots and datasets the stream does not have are
// destroyed (with Force), snapshots that were renamed are renamed, datasets
// that moved are moved, and clones that were promoted on the sender are
// promoted. It reports whether something could not be done. Names of the
// datasets it renamed are appended to renamed if that is not nil.
func (c *Client) reconcile(p *Package, tofs string, o *ReceiveOptions, renamed *[]string) (soft bool) {
	isPrefix := o.Layout != LayoutExact
	for {
		needAgain, progress, err := c.reconcilePass(p, tofs, o, isPrefix, renamed)
		if err != nil {
			return true
		}
		if !(needAgain && progress) {
			return needAgain
		}
	}
}

func (c *Client) reconcilePass(p *Package, tofs string, o *ReceiveOptions, isPrefix bool, renamed *[]string) (needAgain, progress bool, err error) {
	r := &replSend{c: c, top: tofs, from: p.FromSnap, recursive: p.Recursive(), raw: true, props: true, bySnap: map[uint64]*fsEntry{}}
	if err := r.gather(); err != nil {
		return true, false, err
	}
	deleted := map[uint64]bool{}
	baseLen := len(tofs) + 1

	for _, e := range r.entries {
		snaps, _ := e.nv.List("snaps")
		fsname := e.name
		parentFrom := e.parentFrom
		originGUID := e.originGUID

		// The stream's dataset: the first of our snapshots it knows.
		var streamFS *nvlist.List
		if snaps != nil {
			for _, sp := range snaps.Pairs() {
				if g, ok := sp.Value.(uint64); ok {
					if fs, _, ok := p.find(g); ok {
						streamFS = fs
						break
					}
				}
			}
		}

		// A different origin on the sender means a clone was promoted there.
		var streamOrigin uint64
		if streamFS != nil {
			streamOrigin, _ = streamFS.Uint64("origin")
		}
		if streamFS != nil && originGUID != streamOrigin {
			switch r.createdBefore(streamOrigin, originGUID) {
			case 1:
				if oe := r.bySnap[originGUID]; oe != nil {
					if c.Promote(fsname) == nil {
						progress = true
					}
				}
			case -1:
				return true, progress, errors.New("cannot compare the creation of the origins")
			}
			// Our list of snapshots is wrong now: another pass.
			needAgain = true
			continue
		}

		var fromGUID uint64
		var stream *nvlist.List = streamFS
		if snaps != nil {
			for _, sp := range snaps.Pairs() {
				thisGUID, _ := sp.Value.(uint64)
				found, streamSnap, ok := p.find(thisGUID)
				if !ok {
					// The stream no longer has this snapshot.
					if !o.Force {
						continue
					}
					full := fsname + "@" + sp.Name
					if err := c.recvDestroy(full, len(fsname)+1); err != nil {
						needAgain = true
					} else {
						progress = true
					}
					deleted[thisGUID] = true
					continue
				}
				stream = found
				if sp, ok := found.List("snapprops"); ok {
					if props, ok := sp.List(streamSnap); ok {
						c.setReceivedProps(fsname+"@"+snapNameOf(snaps, thisGUID), props)
					}
				}
				if local := snapNameOf(snaps, thisGUID); local != streamSnap {
					if _, err := c.recvRename(fsname+"@"+local, fsname+"@"+streamSnap, len(fsname)+1); err != nil {
						needAgain = true
					} else {
						progress = true
					}
				}
				if streamSnap == p.FromSnap {
					fromGUID = thisGUID
				}
			}
		}

		if stream == nil {
			// The sender no longer has the dataset.
			if !o.Force {
				continue
			}
			if err := c.recvDestroy(fsname, baseLen); err != nil {
				needAgain = true
			} else {
				progress = true
			}
			deleted[parentFrom] = true
			continue
		}
		if fromGUID == 0 {
			continue // we do not have the base snapshot: deleted locally; ignore
		}

		streamName, _ := stream.String("name")
		streamParent, _ := stream.Uint64("parentfromsnap")

		// The parent was destroyed above: renaming under it cannot work, try
		// again once the new parent is known.
		if streamParent != 0 && parentFrom != 0 && streamParent != parentFrom && deleted[parentFrom] {
			progress, needAgain = true, true
			continue
		}

		s1, s2 := lastComponent(fsname), lastComponent(streamName)
		if (streamParent != 0 && parentFrom != 0 && streamParent != parentFrom) ||
			((isPrefix || tofs != fsname) && s1 != "" && s2 != "" && s1 != s2) {
			try := ""
			if pe := r.bySnap[streamParent]; pe != nil {
				try = pe.name + s2
			}
			newName, err := c.recvRename(fsname, try, baseLen)
			if renamed != nil && newName != "" {
				*renamed = append(*renamed, newName)
			}
			if err != nil {
				needAgain = true
			} else {
				progress = true
			}
		}
	}
	return needAgain, progress, nil
}

// snapNameOf returns the name a snapshot of our tree has, given its GUID.
func snapNameOf(snaps *nvlist.List, guid uint64) string {
	for _, sp := range snaps.Pairs() {
		if g, _ := sp.Value.(uint64); g == guid {
			return sp.Name
		}
	}
	return ""
}

// lastComponent is the part of a dataset name from its last slash on.
func lastComponent(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i:]
	}
	return ""
}

// createdBefore is created_before: +1 when the snapshot guid1 is newer than
// guid2, -1 when it is older or cannot be found, 0 when equal; a zero guid2
// gives 0 and a zero guid1 gives 1.
func (r *replSend) createdBefore(guid1, guid2 uint64) int {
	if guid2 == 0 {
		return 0
	}
	if guid1 == 0 {
		return 1
	}
	txg := func(g uint64) (uint64, bool) {
		e := r.bySnap[g]
		if e == nil {
			return 0, false
		}
		for _, s := range e.snaps {
			if s.guid == g {
				return s.txg, true
			}
		}
		return 0, false
	}
	t1, ok1 := txg(guid1)
	t2, ok2 := txg(guid2)
	if !ok1 || !ok2 {
		return -1
	}
	switch {
	case t1 < t2:
		return -1
	case t1 > t2:
		return 1
	}
	return 0
}

// recvRename is recv_rename: rename name to try; if that fails, or there is
// no name to try, move it out of the way to "recv-<pid>-<n>" (so that another
// rename can take its place) and report EAGAIN. newName is the name it was
// moved to, if it moved.
func (c *Client) recvRename(name, try string, baseLen int) (newName string, err error) {
	if try != "" {
		err = c.Rename(name, try, false)
		if err == nil {
			return try, nil
		}
	} else {
		err = syscall.ENOENT
	}
	if baseLen <= len(name) && !strings.HasPrefix(name[baseLen:], "recv-") {
		tmp := fmt.Sprintf("%srecv-%d-%d", name[:baseLen], os.Getpid(), recvSeq.Add(1))
		if c.Rename(name, tmp, false) == nil {
			newName = tmp
		}
		return newName, syscall.EAGAIN
	}
	return "", err
}

// recvDestroy is recv_destroy: destroy a snapshot (deferred, if the pool can)
// or a dataset; when that does not remove it, move it out of the way.
func (c *Client) recvDestroy(name string, baseLen int) error {
	var err error
	snapshot := strings.Contains(name, "@")
	if snapshot {
		err = c.DestroySnapshots([]string{name}, true)
	} else {
		err = c.Destroy(name, nil)
	}
	gone := err == nil
	if gone && snapshot {
		if ok, _ := c.Exists(name); ok {
			gone = false // only marked for destruction
		}
	}
	if gone {
		return nil
	}
	_, rerr := c.recvRename(name, "", baseLen)
	if rerr == nil {
		rerr = err
	}
	return rerr
}
