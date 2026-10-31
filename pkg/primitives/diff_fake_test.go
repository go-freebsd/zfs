package primitives

import (
	"encoding/binary"
	"sort"
	"syscall"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// What the fake kernel knows for zfs diff: the records ZFS_IOC_DIFF writes,
// and per snapshot the objects (path, stat) it can describe.
type fakeObj struct {
	path  string
	stat  ioctl.Stat
	errno syscall.Errno // the lookup fails with it
}

type diffRec struct{ typ, first, last uint64 }

func (k *fakeKernel) diffIoctl(ioc ioctl.IOC, cmd *ioctl.Cmd) (bool, error) {
	switch ioc {
	case ioctl.IOCTmpSnapshot:
		cmd.SetValue("zfs-diff-1")
		k.tmpSnaps = append(k.tmpSnaps, cmd.GetName()+"@zfs-diff-1")
		return true, nil
	case ioctl.IOCObjToStats:
		o, ok := k.objs[cmd.GetName()][cmd.Obj]
		if !ok {
			return true, k.fail(ioc, cmd.GetName(), syscall.ENOENT)
		}
		cmd.Stat = o.stat
		if o.errno != 0 {
			return true, k.fail(ioc, cmd.GetName(), o.errno)
		}
		cmd.SetValue(o.path)
		return true, nil
	case ioctl.IOCNextObj:
		var keys []uint64
		for o := range k.objs[cmd.GetName()] {
			if o > cmd.Obj {
				keys = append(keys, o)
			}
		}
		if len(keys) == 0 {
			return true, k.fail(ioc, cmd.GetName(), syscall.ESRCH)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		cmd.Obj = keys[0]
		return true, nil
	}
	return false, nil
}

func (k *fakeKernel) DoOnce(ioc ioctl.IOC, cmd *ioctl.Cmd, in *nvlist.List, dstSize int) (*nvlist.List, error) {
	k.n++
	k.calls = append(k.calls, ioc.String()+" "+cmd.GetName())
	if ioc != ioctl.IOCDiff {
		k.t.Fatalf("fake: unexpected DoOnce(%v)", ioc)
	}
	k.diffArgs = append(k.diffArgs, [2]string{cmd.GetValue(), cmd.GetName()})
	if e, ok := k.writeErr[ioc]; ok {
		return nil, e
	}
	fd := int(cmd.Cookie)
	for _, r := range k.diffRecords {
		var b [24]byte
		binary.NativeEndian.PutUint64(b[0:], r.typ)
		binary.NativeEndian.PutUint64(b[8:], r.first)
		binary.NativeEndian.PutUint64(b[16:], r.last)
		for p := b[:]; len(p) > 0; {
			n, err := syscall.Write(fd, p)
			if err != nil {
				return nil, &ioctl.Error{Ioc: ioc, Name: cmd.GetName(), Errno: err.(syscall.Errno)}
			}
			p = p[n:]
		}
	}
	return nil, nil
}
