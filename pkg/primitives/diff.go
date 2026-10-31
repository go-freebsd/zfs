package primitives

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// DiffChange is the kind of change a DiffEntry reports, as the first column
// of zfs diff.
type DiffChange byte

const (
	DiffAdded    DiffChange = '+'
	DiffRemoved  DiffChange = '-'
	DiffModified DiffChange = 'M'
	DiffRenamed  DiffChange = 'R'
)

// DiffEntry is one line of zfs diff.
type DiffEntry struct {
	Change DiffChange
	// Type is the file type character of zfs diff -F: 'F' file, '/'
	// directory, '@' symbolic link, '|' FIFO, '=' socket, 'B' block and 'C'
	// character device, '?' anything else.
	Type byte
	// Path is the path inside the dataset, starting with a slash; NewPath
	// is the new name of a renamed object.
	Path, NewPath string
	// Ctime is the change time of the object (zfs diff -t).
	Ctime time.Time
	// LinkDelta is not zero for a hard link that was added or removed: the
	// entry is a modification with the change in the link count, shown by
	// zfs diff as "(+1)".
	LinkDelta int
}

// dmu_diff_record_t: ddr_type is DDR_NONE (1), DDR_INUSE or DDR_FREE.
const (
	ddrInUse = 2
	ddrFree  = 4
	ddrSize  = 24
)

// objects of one dataset are described through snapshots; this is what the
// kernel tells about one.
type diffObject struct {
	path string
	stat ioctl.Stat
	err  error // nil, or the errno of the lookup (the stat is valid for ESTALE)
}

// Diff lists what changed between the snapshot from and the snapshot to of
// the same dataset, or between from and the live filesystem when to is empty
// (zfs diff). Both are full names; to may also be "@name" for a snapshot of
// the dataset of from. A snapshot of a clone of from's dataset is accepted
// as to, as zfs diff does.
//
// Entries come in the order of object numbers, not of path names. Paths are
// not escaped (zfs diff escapes them for display unless -H). For the live
// filesystem a temporary snapshot is made and removed when the iteration
// ends, which needs the same privileges as zfs diff.
func (c *Client) Diff(ctx context.Context, from, to string) iter.Seq2[DiffEntry, error] {
	return func(yield func(DiffEntry, error) bool) {
		ds, snap, ok := strings.Cut(from, "@")
		if !ok || ds == "" || snap == "" || strings.ContainsAny(snap, "@#/") {
			yield(DiffEntry{}, fmt.Errorf("zfs: diff: %q is not a snapshot name", from))
			return
		}
		var keep func()
		var err error
		if to == "" {
			if to, keep, err = c.tempSnapshot(ds); err != nil {
				yield(DiffEntry{}, err)
				return
			}
			defer keep()
		} else if strings.HasPrefix(to, "@") {
			to = ds + to
		}
		if i := strings.IndexByte(to, '@'); i <= 0 || i == len(to)-1 {
			yield(DiffEntry{}, fmt.Errorf("zfs: diff: %q is not a snapshot name", to))
			return
		}
		c.runDiff(ctx, from, to, yield)
	}
}

// tempSnapshot makes the snapshot of the live filesystem that zfs diff
// compares against. The kernel destroys it when the descriptor it was made
// with is closed.
func (c *Client) tempSnapshot(ds string) (name string, release func(), err error) {
	cmd := ioctl.NewCmd()
	if err := cmd.SetName(ds); err != nil {
		return "", nil, err
	}
	if err := cmd.SetValue(fmt.Sprintf("zfs-diff-%d", os.Getpid())); err != nil {
		return "", nil, err
	}
	release = func() {}
	if c.conn != nil {
		// The temporary snapshot lives as long as this descriptor.
		own, err := ioctl.Open()
		if err != nil {
			return "", nil, err
		}
		cmd.CleanupFD = int32(own.FD())
		release = func() { own.Close() }
	}
	if err := c.c.Ioctl(ioctl.IOCTmpSnapshot, cmd); err != nil {
		release()
		return "", nil, fmt.Errorf("zfs: diff: cannot create a just-in-time snapshot of %q: %w", ds, err)
	}
	return ds + "@" + cmd.GetValue(), release, nil
}

func (c *Client) runDiff(ctx context.Context, from, to string, yield func(DiffEntry, error) bool) {
	pr, pw, err := os.Pipe()
	if err != nil {
		yield(DiffEntry{}, err)
		return
	}
	ioDone := make(chan error, 1)
	go func() {
		cmd := ioctl.NewCmd()
		cmd.SetName(to)
		cmd.SetValue(from)
		cmd.Cookie = uint64(pw.Fd()) // blocking, as the kernel needs it
		// Once: the kernel writes while the call runs, a repeat would write twice.
		_, err := c.c.DoOnce(ioctl.IOCDiff, cmd, nil, 0)
		pw.Close()
		ioDone <- err
	}()
	stop := func() {
		pr.Close() // the kernel's next write fails with EPIPE
		<-ioDone
	}

	d := &differ{c: c, from: from, to: to}
	var rec [ddrSize]byte
	for {
		if err := ctx.Err(); err != nil {
			stop()
			yield(DiffEntry{}, err)
			return
		}
		if _, err := io.ReadFull(pr, rec[:]); err != nil {
			pr.Close()
			ioErr := <-ioDone
			switch {
			case ioErr != nil:
				yield(DiffEntry{}, diffIoctlError(ioErr))
			case errors.Is(err, io.ErrUnexpectedEOF):
				yield(DiffEntry{}, errors.New("zfs: diff: bad data from the diff ioctl"))
			}
			return
		}
		typ := binary.NativeEndian.Uint64(rec[0:])
		first, last := binary.NativeEndian.Uint64(rec[8:]), binary.NativeEndian.Uint64(rec[16:])
		var err error
		switch typ {
		case ddrFree:
			err = d.free(first, last, yield)
		case ddrInUse:
			for o := first; o <= last && err == nil; o++ {
				err = d.inUse(o, yield)
			}
		default:
			err = errors.New("zfs: diff: bad data from the diff ioctl")
		}
		if err != nil {
			stop()
			if err != errStopped {
				yield(DiffEntry{}, err)
			}
			return
		}
	}
}

var errStopped = errors.New("stopped")

func diffIoctlError(err error) error {
	switch {
	case errors.Is(err, syscall.EPERM):
		return fmt.Errorf("zfs: diff: the sys_mount privilege or diff delegated permission is needed to execute the diff ioctl: %w", err)
	case errors.Is(err, syscall.EXDEV):
		return fmt.Errorf("zfs: diff: not an earlier snapshot from the same fs: %w", err)
	}
	return fmt.Errorf("zfs: diff: unable to obtain diffs: %w", err)
}

// differ ports libzfs_diff.c: the kernel names the objects that changed, and
// the two snapshots tell what they were.
type differ struct {
	c        *Client
	from, to string
}

func (d *differ) lookup(snapshot string, obj uint64) diffObject {
	cmd := ioctl.NewCmd()
	cmd.SetName(snapshot)
	cmd.Obj = obj
	err := d.c.c.Ioctl(ioctl.IOCObjToStats, cmd)
	// Stats come back even when the path could not be made.
	o := diffObject{stat: cmd.Stat}
	if err == nil {
		o.path = cmd.GetValue()
		return o
	}
	o.err = err
	return o
}

// objectError turns a failed lookup into the error zfs diff reports, or nil
// for the cases it tolerates.
func (d *differ) objectError(snapshot string, obj uint64, o *diffObject) error {
	switch {
	case o.err == nil:
		return nil
	case errors.Is(o.err, syscall.ESTALE):
		o.path, o.err = "(on_delete_queue)", nil
		return nil
	case errors.Is(o.err, syscall.EPERM):
		return fmt.Errorf("zfs: diff: the sys_config privilege or diff delegated permission is needed to discover path names: %w", o.err)
	case errors.Is(o.err, syscall.EACCES):
		return fmt.Errorf("zfs: diff: key must be loaded to discover path names: %w", o.err)
	}
	return fmt.Errorf("zfs: diff: unable to determine path or stats for object %d in %s: %w", obj, snapshot, o.err)
}

func absent(o diffObject) bool {
	return errors.Is(o.err, syscall.ENOENT) || errors.Is(o.err, syscall.ENOTSUP)
}

const sIFMT = 0o170000

func fileType(mode uint64) byte {
	switch mode & sIFMT {
	case 0o060000:
		return 'B'
	case 0o020000:
		return 'C'
	case 0o040000:
		return '/'
	case 0o010000:
		return '|'
	case 0o120000:
		return '@'
	case 0o140000:
		return '='
	case 0o100000:
		return 'F'
	}
	return '?'
}

func entry(ch DiffChange, path string, st ioctl.Stat) DiffEntry {
	return DiffEntry{
		Change: ch,
		Type:   fileType(st.Mode),
		Path:   path,
		Ctime:  time.Unix(int64(st.Ctime[0]), int64(st.Ctime[1])),
	}
}

func (d *differ) emit(yield func(DiffEntry, error) bool, e DiffEntry) error {
	if !yield(e, nil) {
		return errStopped
	}
	return nil
}

// inUse is write_inuse_diffs_one.
func (d *differ) inUse(obj uint64, yield func(DiffEntry, error) bool) error {
	f := d.lookup(d.from, obj)
	var fErr error
	if f.err != nil && !absent(f) {
		fErr = d.objectError(d.from, obj, &f)
	}
	t := d.lookup(d.to, obj)
	var tErr error
	if t.err != nil && !absent(t) {
		tErr = d.objectError(d.to, obj, &t)
	}
	// An unallocated object that shares a block of the meta dnode.
	if f.err != nil && t.err != nil && absent(f) && absent(t) {
		return nil
	}
	if fErr != nil {
		return fErr
	}
	if tErr != nil {
		return tErr
	}

	fmode, tmode := f.stat.Mode&sIFMT, t.stat.Mode&sIFMT
	change := 0
	if !(fmode == 0o040000 || tmode == 0o040000 || f.stat.Links == 0 || t.stat.Links == 0) {
		change = int(t.stat.Links) - int(f.stat.Links)
	}
	fAbsent, tAbsent := f.err != nil, t.err != nil
	linkChange := func(path string, st ioctl.Stat) error {
		e := entry(DiffModified, path, st)
		e.LinkDelta = change
		return d.emit(yield, e)
	}
	switch {
	case fAbsent:
		if change != 0 {
			return linkChange(t.path, t.stat)
		}
		return d.emit(yield, entry(DiffAdded, t.path, t.stat))
	case tAbsent:
		if change != 0 {
			return linkChange(f.path, f.stat)
		}
		return d.emit(yield, entry(DiffRemoved, f.path, f.stat))
	}

	if fmode != tmode && f.stat.Gen == t.stat.Gen {
		t.stat.Gen++ // force a generational difference
	}
	if f.stat.Gen == t.stat.Gen {
		if f.stat.Ctime == t.stat.Ctime {
			return nil // no apparent change
		}
		switch {
		case change != 0:
			if change > 0 {
				return linkChange(f.path, t.stat)
			}
			return linkChange(t.path, t.stat)
		case f.path == t.path:
			return d.emit(yield, entry(DiffModified, f.path, t.stat))
		default:
			e := entry(DiffRenamed, f.path, t.stat)
			e.NewPath = t.path
			return d.emit(yield, e)
		}
	}
	// The file was re-created, or the object number reused.
	if err := d.emit(yield, entry(DiffRemoved, f.path, f.stat)); err != nil {
		return err
	}
	return d.emit(yield, entry(DiffAdded, t.path, t.stat))
}

// free is write_free_diffs: objects freed in the range existed in from.
func (d *differ) free(first, last uint64, yield func(DiffEntry, error) bool) error {
	cmd := ioctl.NewCmd()
	cmd.SetName(d.from)
	cmd.Obj = first - 1
	for cmd.Obj < last {
		err := d.c.c.Ioctl(ioctl.IOCNextObj, cmd)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("zfs: diff: next allocated object (> %d) find failure: %w", cmd.Obj, err)
		}
		if cmd.Obj > last {
			return nil
		}
		o := d.lookup(d.from, cmd.Obj)
		if errors.Is(o.err, syscall.ESTALE) || errors.Is(o.err, syscall.ENOENT) {
			continue // on the delete queue of the from side
		}
		if err := d.objectError(d.from, cmd.Obj, &o); err != nil {
			return err
		}
		if err := d.emit(yield, entry(DiffRemoved, o.path, o.stat)); err != nil {
			return err
		}
	}
	return nil
}
