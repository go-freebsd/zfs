package primitives

import (
	"sort"
	"syscall"
	"testing"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// fakeKernel is an in-memory pool that answers the read ioctls the way the
// kernel does, including the cursor protocol of the *_LIST_NEXT calls.
type fakeKernel struct {
	t *testing.T

	ds                map[string]*fakeDS  // every dataset and snapshot by full name
	children          map[string][]string // parent -> children in "ZAP order" (unsorted on purpose)
	snaps             map[string][]string // dataset -> snapshot names in kernel order
	pools             []string
	bareBookmarkProps bool // answer GET_BOOKMARKS with bare values instead of {value} lists

	iters   map[uint64]string // cursor token -> parent it was started for
	nextTok uint64
	calls   []string

	writes   []writeCall         // create/destroy/rename/... requests received
	writeErr map[ioctl.IOC]error // errors to return for them
	writeOut map[ioctl.IOC]*nvlist.List
	sendData map[string][]byte // stream bytes a send of a snapshot (or "from>to") writes
	received [][]byte          // payload bytes of each received stream
	progress uint64
	sets     []setCall                // SET_PROP requests received
	inherits []inheritCall            // INHERIT_PROP requests received
	setFail  map[string]syscall.Errno // properties the "kernel" refuses

	// after, if set, runs after a write or SET_PROP call succeeded; tests use
	// it to make the change a real kernel would make.
	after func(w writeCall)

	// hook, if set, runs before a call is served; a non-nil error is returned.
	hook func(ioc ioctl.IOC, cmd *ioctl.Cmd, n int) error
	n    int
}

type writeCall struct {
	ioc    ioctl.IOC
	name   string
	value  string // zc_value
	cookie uint64
	in     *nvlist.List
}

func isWriteIOC(ioc ioctl.IOC) bool {
	switch ioc {
	case ioctl.IOCCreate, ioctl.IOCDestroy, ioctl.IOCRollback, ioctl.IOCRename, ioctl.IOCPromote,
		ioctl.IOCSnapshot, ioctl.IOCHold, ioctl.IOCRelease, ioctl.IOCDestroySnaps, ioctl.IOCClone,
		ioctl.IOCBookmark, ioctl.IOCDestroyBookmarks, ioctl.IOCLoadKey, ioctl.IOCUnloadKey, ioctl.IOCChangeKey,
		ioctl.IOCSendNew, ioctl.IOCRecvNew, ioctl.IOCSendSpace, ioctl.IOCRedact, ioctl.IOCSendProgress:
		return true
	}
	return false
}

// write records a write ioctl and answers it from writeErr/writeOut.
func (k *fakeKernel) write(ioc ioctl.IOC, cmd *ioctl.Cmd, in *nvlist.List) (*nvlist.List, error) {
	k.writes = append(k.writes, writeCall{ioc, cmd.GetName(), cmd.GetValue(), cmd.Cookie, in})
	if err := k.writeErr[ioc]; err != nil {
		return nil, err
	}
	if k.after != nil {
		k.after(k.writes[len(k.writes)-1])
	}
	return k.writeOut[ioc], nil
}

type setCall struct {
	dataset string
	props   *nvlist.List
	cookie  uint64 // zc_cookie: set for "received" values
}

type inheritCall struct{ dataset, prop string }

type fakeDS struct {
	name      string
	osType    uint32 // dmu_objset_type_t
	snapshot  bool
	guid, txg uint64
	origin    string
	clones    uint64
	props     map[string]Property
	holds     map[string]uint64
	bookmarks []fakeBookmark
}

type fakeBookmark struct {
	name                string
	guid, txg, creation uint64
}

func (k *fakeKernel) add(d *fakeDS) *fakeDS {
	if d.props == nil {
		d.props = map[string]Property{}
	}
	k.ds[d.name] = d
	return d
}

func (k *fakeKernel) fs(name string, txg uint64) *fakeDS {
	return k.add(&fakeDS{name: name, osType: 2, guid: 1000 + txg, txg: txg, props: map[string]Property{
		"used":        {Value: uint64(100 * txg)},
		"available":   {Value: uint64(1 << 20)},
		"referenced":  {Value: uint64(10 * txg)},
		"mountpoint":  {Value: "/" + name, Source: "default"},
		"compression": {Value: uint64(15), Source: name},
		"creation":    {Value: uint64(1700000000 + txg)},
	}})
}

func (k *fakeKernel) snap(of string, tag string, txg uint64) *fakeDS {
	name := of + "@" + tag
	d := k.add(&fakeDS{name: name, osType: 2, snapshot: true, guid: 2000 + txg, txg: txg,
		props: map[string]Property{"used": {Value: uint64(0)}, "creation": {Value: uint64(1700000000 + txg)}}})
	k.snaps[of] = append(k.snaps[of], name)
	return d
}

func newFake(t *testing.T) *fakeKernel {
	k := &fakeKernel{t: t, ds: map[string]*fakeDS{}, children: map[string][]string{}, snaps: map[string][]string{},
		iters: map[uint64]string{}, pools: []string{"tank"}}

	k.fs("tank", 1)
	a := k.fs("tank/a", 10)
	k.fs("tank/a/child", 11)
	k.fs("tank/z", 12)
	vol := k.fs("tank/vol", 13)
	vol.osType = 3
	vol.props["volsize"] = Property{Value: uint64(8 << 20)}
	clone := k.fs("tank/clone", 14)

	// Kernel order is not sorted order.
	k.children["tank"] = []string{"tank/z", "tank/vol", "tank/a", "tank/clone"}
	k.children["tank/a"] = []string{"tank/a/child"}

	// Snapshots in an order that is neither name nor creation order.
	k.snap("tank/a", "s2", 20)
	s1 := k.snap("tank/a", "s1", 15)
	k.snap("tank/a", "s3", 30)
	s1.holds = map[string]uint64{"keep": 1700000100, "other": 1700000200}
	clone.origin = "tank/a@s1"
	s1.clones = 1
	cl := nvlist.New()
	cl.Add("tank/clone", nvlist.Flag{})
	s1.props["clones"] = Property{Value: cl}
	a.bookmarks = []fakeBookmark{{"bm2", 7002, 25, 1700000300}, {"bm1", 7001, 17, 1700000250}}
	k.snap("tank/vol", "v1", 16)
	return k
}

func (k *fakeKernel) fail(ioc ioctl.IOC, name string, e syscall.Errno) error {
	return &ioctl.Error{Ioc: ioc, Name: name, Errno: e}
}

const idxBits = 20

func (k *fakeKernel) fillStats(cmd *ioctl.Cmd, d *fakeDS) {
	st := &cmd.ObjsetStats
	*st = ioctl.ObjsetStats{Type: d.osType, GUID: d.guid, CreationTxg: d.txg, NumClones: d.clones}
	if d.snapshot {
		st.IsSnapshot = 1
	}
	copy(st.Origin[:], d.origin)
}

func (k *fakeKernel) propsList(d *fakeDS) *nvlist.List {
	l, err := nvlist.Encode(d.props)
	if err != nil {
		k.t.Fatalf("fake: encoding properties of %s: %v", d.name, err)
	}
	return l
}

func (k *fakeKernel) Do(ioc ioctl.IOC, cmd *ioctl.Cmd, in *nvlist.List) (*nvlist.List, error) {
	k.n++
	k.calls = append(k.calls, ioc.String()+" "+cmd.GetName())
	if k.hook != nil {
		if err := k.hook(ioc, cmd, k.n); err != nil {
			return nil, err
		}
	}
	if isWriteIOC(ioc) {
		return k.write(ioc, cmd, in)
	}
	switch ioc {
	case ioctl.IOCObjsetStats:
		d, ok := k.ds[cmd.GetName()]
		if !ok {
			return nil, k.fail(ioc, cmd.GetName(), syscall.ENOENT)
		}
		k.fillStats(cmd, d)
		if cmd.Simple != 0 {
			return nil, nil
		}
		return k.propsList(d), nil

	case ioctl.IOCDatasetListNext, ioctl.IOCSnapshotListNext:
		return k.listNext(ioc, cmd, in)

	case ioctl.IOCSetProp:
		name := cmd.GetName()
		if _, ok := k.ds[name]; !ok {
			return nil, k.fail(ioc, name, syscall.ENOENT)
		}
		k.sets = append(k.sets, setCall{name, in, cmd.Cookie})
		defer func() {
			if k.after != nil {
				k.after(writeCall{ioc: ioc, name: name, in: in})
			}
		}()
		var refused *nvlist.List
		for _, p := range in.Names() {
			if e, bad := k.setFail[p]; bad {
				if refused == nil {
					refused = nvlist.New()
				}
				refused.Add(p, int32(e))
			}
		}
		if refused != nil {
			// As the kernel does: the failing call still returns the list.
			return nil, &ioctl.Error{Ioc: ioc, Name: name, Errno: syscall.EINVAL, Detail: refused}
		}
		return nil, nil

	case ioctl.IOCPoolConfigs:
		out := nvlist.New()
		for _, p := range k.pools {
			out.Add(p, nvlist.New())
		}
		cmd.Cookie++
		return out, nil
	}
	k.t.Fatalf("fake: unexpected Do(%v)", ioc)
	return nil, nil
}

// listNext implements the cursor protocol: cookie 0 starts an iteration, the
// kernel advances the cookie, and every later call must name the same parent.
func (k *fakeKernel) listNext(ioc ioctl.IOC, cmd *ioctl.Cmd, in *nvlist.List) (*nvlist.List, error) {
	parent := cmd.GetName()
	var names []string
	if ioc == ioctl.IOCDatasetListNext {
		names = k.children[parent]
	} else {
		names = k.snaps[parent]
	}
	if _, ok := k.ds[parent]; !ok {
		return nil, k.fail(ioc, parent, syscall.ENOENT)
	}

	tok, idx := cmd.Cookie>>idxBits, int(cmd.Cookie&(1<<idxBits-1))
	if cmd.Cookie == 0 {
		k.nextTok++
		tok = k.nextTok
		k.iters[tok] = parent
	} else if k.iters[tok] != parent {
		k.t.Errorf("fake: %v continued a cursor of %q with zc_name %q: the caller must reset zc_name to the parent before every call",
			ioc, k.iters[tok], parent)
		return nil, k.fail(ioc, parent, syscall.EINVAL)
	}

	var minTxg, maxTxg uint64
	if in != nil {
		minTxg, _ = in.Uint64("snap_iter_min_txg")
		maxTxg, _ = in.Uint64("snap_iter_max_txg")
	}
	for ; idx < len(names); idx++ {
		d := k.ds[names[idx]]
		if ioc == ioctl.IOCSnapshotListNext && ((minTxg != 0 && d.txg < minTxg) || (maxTxg != 0 && d.txg > maxTxg)) {
			continue
		}
		cmd.Cookie = tok<<idxBits | uint64(idx+1)
		cmd.Obj = uint64(idx + 1)
		cmd.SetName(d.name)
		k.fillStats(cmd, d)
		if cmd.Simple != 0 {
			return nil, nil
		}
		return k.propsList(d), nil
	}
	return nil, k.fail(ioc, parent, syscall.ESRCH)
}

func (k *fakeKernel) Call(ioc ioctl.IOC, name string, in *nvlist.List) (*nvlist.List, error) {
	k.n++
	k.calls = append(k.calls, ioc.String()+" "+name)
	if k.hook != nil {
		if err := k.hook(ioc, ioctl.NewCmd(), k.n); err != nil {
			return nil, err
		}
	}
	if isWriteIOC(ioc) {
		cmd := ioctl.NewCmd()
		cmd.SetName(name)
		return k.write(ioc, cmd, in)
	}
	d, ok := k.ds[name]
	if !ok {
		return nil, k.fail(ioc, name, syscall.ENOENT)
	}
	switch ioc {
	case ioctl.IOCGetHolds:
		out := nvlist.New()
		tags := make([]string, 0, len(d.holds))
		for tag := range d.holds {
			tags = append(tags, tag)
		}
		sort.Strings(tags)
		for _, tag := range tags {
			out.Add(tag, d.holds[tag])
		}
		return out, nil

	case ioctl.IOCGetBookmarks:
		out := nvlist.New()
		for _, b := range d.bookmarks {
			props := nvlist.New()
			for _, want := range in.Names() {
				var v uint64
				switch want {
				case "guid":
					v = b.guid
				case "createtxg":
					v = b.txg
				case "creation":
					v = b.creation
				default:
					continue
				}
				if k.bareBookmarkProps {
					props.Add(want, v)
				} else {
					p := nvlist.New()
					p.Add("value", v)
					props.Add(want, p)
				}
			}
			out.Add(b.name, props)
		}
		return out, nil

	case ioctl.IOCSpaceSnaps:
		first, _ := in.String("firstsnap")
		if _, ok := k.ds[first]; !ok {
			return nil, k.fail(ioc, first, syscall.ENOENT)
		}
		out := nvlist.New()
		out.Add("used", uint64(4096))
		out.Add("compressed", uint64(2048))
		out.Add("uncompressed", uint64(8192))
		return out, nil
	}
	k.t.Fatalf("fake: unexpected Call(%v)", ioc)
	return nil, nil
}

// CallOnce serves send and receive: it moves bytes through the descriptor
// the request names, like the kernel does.
func (k *fakeKernel) CallOnce(ioc ioctl.IOC, name string, in *nvlist.List) (*nvlist.List, error) {
	k.n++
	k.calls = append(k.calls, ioc.String()+" "+name)
	cmd := ioctl.NewCmd()
	cmd.SetName(name)
	out, err := k.write(ioc, cmd, in)
	if err != nil {
		return nil, err
	}
	switch ioc {
	case ioctl.IOCSendNew:
		fd, _ := in.Int32("fd")
		data := k.sendData[name]
		if from, ok := in.String("fromsnap"); ok {
			data = k.sendData[from+">"+name]
		}
		for len(data) > 0 {
			n, werr := syscall.Write(int(fd), data)
			if werr != nil {
				return nil, &ioctl.Error{Ioc: ioc, Name: name, Errno: werr.(syscall.Errno)}
			}
			data = data[n:]
		}
	case ioctl.IOCRecvNew:
		fd, _ := in.Int32("input_fd")
		rec, _ := in.Get("begin_record")
		bytes := rec.([]nvlist.Byte)
		want := int(bytes[4]) | int(bytes[5])<<8 | int(bytes[6])<<16 | int(bytes[7])<<24 // drr_payloadlen, little endian host
		body := make([]byte, 0, want)
		buf := make([]byte, 4096)
		for len(body) < want {
			n, rerr := syscall.Read(int(fd), buf[:min(len(buf), want-len(body))])
			if rerr != nil || n == 0 {
				return nil, &ioctl.Error{Ioc: ioc, Name: name, Errno: syscall.EPIPE}
			}
			body = append(body, buf[:n]...)
		}
		k.received = append(k.received, body)
		if out == nil {
			out = nvlist.New()
			out.Add("read_bytes", uint64(want+312))
			out.Add("error_flags", uint64(0))
			out.Add("errors", nvlist.New())
		}
	}
	return out, nil
}

func (k *fakeKernel) Ioctl(ioc ioctl.IOC, cmd *ioctl.Cmd) error {
	k.n++
	k.calls = append(k.calls, ioc.String()+" "+cmd.GetName())
	if ioc == ioctl.IOCSendProgress {
		k.writes = append(k.writes, writeCall{ioc, cmd.GetName(), "", cmd.Cookie, nil})
		cmd.Cookie, cmd.ObjsetType = k.progress, k.progress/512
		return k.writeErr[ioc]
	}
	if isWriteIOC(ioc) {
		_, err := k.write(ioc, cmd, nil)
		return err
	}
	if _, ok := k.ds[cmd.GetName()]; !ok {
		return k.fail(ioc, cmd.GetName(), syscall.ENOENT)
	}
	switch ioc {
	case ioctl.IOCSpaceWritten:
		cmd.Cookie, cmd.ObjsetType, cmd.PermAction = 1000, 500, 2000
	case ioctl.IOCInheritProp:
		k.inherits = append(k.inherits, inheritCall{cmd.GetName(), cmd.GetValue()})
		if k.after != nil {
			k.after(writeCall{ioc: ioc, name: cmd.GetName(), value: cmd.GetValue()})
		}
	default:
		k.t.Fatalf("fake: unexpected Ioctl(%v)", ioc)
	}
	return nil
}
