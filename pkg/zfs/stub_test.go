package zfs

import (
	"context"
	"fmt"
	"io"
	"iter"
	"sort"

	"github.com/go-freebsd/zfs/pkg/primitives"
)

// stub is a backend that records what it is asked to do.
type stub struct {
	calls []string // "Method arg arg ..."

	// recorded arguments of the last call of each kind
	createOpts  *primitives.CreateOptions
	setProps    map[string]any
	snapNames   []string
	snapProps   map[string]any
	destroyOpt  *primitives.DestroyOptions
	key         *primitives.Key
	sendOpts    *primitives.SendOptions
	recvOpts    *primitives.ReceiveOptions
	mountOpts   *primitives.MountOptions
	unmountOpts *primitives.UnmountOptions
	writer      io.Writer
	token       string

	// answers
	err      error                 // returned by every call when set
	tree     []*primitives.Dataset // what Walk visits
	get      *primitives.Dataset
	walkErr  error
	walked   int // datasets handed to the Walk callback
	exists   bool
	holds    []primitives.Hold
	space    primitives.Space
	bookmark []*primitives.Dataset
}

func (s *stub) rec(m string, args ...any) error {
	s.calls = append(s.calls, m+" "+fmt.Sprint(args...))
	return s.err
}

func (s *stub) last() string {
	if len(s.calls) == 0 {
		return ""
	}
	return s.calls[len(s.calls)-1]
}

func (s *stub) Close() error { return s.rec("Close") }
func (s *stub) Pools() ([]string, error) {
	return []string{"tank", "zroot"}, s.rec("Pools")
}
func (s *stub) Get(name string, props ...string) (*primitives.Dataset, error) {
	return s.get, s.rec("Get", name)
}
func (s *stub) Exists(name string) (bool, error) { return s.exists, s.rec("Exists", name) }
func (s *stub) Walk(root string, opts primitives.ListOptions, fn func(*primitives.Dataset) error) error {
	s.rec("Walk", root, " types=", opts.Types, " depth=", opts.Depth)
	if s.err != nil {
		return s.err
	}
	for _, d := range s.tree {
		s.walked++
		if err := fn(d); err != nil {
			return err
		}
	}
	return s.walkErr
}
func (s *stub) Bookmarks(fs string, props ...string) ([]*primitives.Dataset, error) {
	return s.bookmark, s.rec("Bookmarks", fs)
}
func (s *stub) Holds(snapshot string) ([]primitives.Hold, error) {
	return s.holds, s.rec("Holds", snapshot)
}
func (s *stub) SpaceWritten(newer, older string) (primitives.Space, error) {
	return s.space, s.rec("SpaceWritten", newer, " ", older)
}
func (s *stub) Create(name string, o *primitives.CreateOptions) error {
	s.createOpts = o
	return s.rec("Create", name)
}
func (s *stub) CreateVolume(name string, size uint64, o *primitives.CreateOptions) error {
	s.createOpts = o
	return s.rec("CreateVolume", name, " ", size)
}
func (s *stub) Clone(name, origin string, o *primitives.CreateOptions) error {
	s.createOpts = o
	return s.rec("Clone", name, " ", origin)
}
func (s *stub) Snapshot(names []string, props map[string]any) error {
	s.snapNames, s.snapProps = names, props
	return s.rec("Snapshot", names)
}
func (s *stub) Destroy(name string, o *primitives.DestroyOptions) error {
	s.destroyOpt = o
	return s.rec("Destroy", name)
}
func (s *stub) Rename(o, n string, r bool) error { return s.rec("Rename", o, " ", n, " ", r) }
func (s *stub) Rollback(ds, snap string) (string, error) {
	return snap, s.rec("Rollback", ds, " ", snap)
}
func (s *stub) Promote(c string) error { return s.rec("Promote", c) }
func (s *stub) Hold(tag string, snaps ...string) error {
	return s.rec("Hold", tag, " ", snaps)
}
func (s *stub) Release(tag string, snaps ...string) error {
	return s.rec("Release", tag, " ", snaps)
}
func (s *stub) CreateBookmark(b, src string) error { return s.rec("CreateBookmark", b, " ", src) }
func (s *stub) SetProps(ds string, p map[string]any) error {
	s.setProps = p
	return s.rec("SetProps", ds)
}
func (s *stub) InheritProp(ds, p string) error { return s.rec("InheritProp", ds, " ", p) }
func (s *stub) LoadKey(ds string, k *primitives.Key) error {
	s.key = k
	return s.rec("LoadKey", ds)
}
func (s *stub) CheckKey(ds string, k *primitives.Key) error { s.key = k; return s.rec("CheckKey", ds) }
func (s *stub) UnloadKey(ds string) error                   { return s.rec("UnloadKey", ds) }
func (s *stub) ChangeKey(ds string, k *primitives.Key) error {
	s.key = k
	return s.rec("ChangeKey", ds)
}
func (s *stub) InheritKey(ds string) error { return s.rec("InheritKey", ds) }

func newStub() (*Client, *stub) {
	s := &stub{}
	return &Client{b: s}, s
}

func keysOf(m map[string]any) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

func ds(name string, kind primitives.Type, props map[string]primitives.Property) *primitives.Dataset {
	return &primitives.Dataset{Name: name, Type: kind, Props: props}
}

// Send and receive.

func (s *stub) Send(ctx context.Context, snapshot string, w io.Writer, o *primitives.SendOptions) error {
	s.sendOpts, s.writer = o, w
	return s.rec("Send", snapshot)
}
func (s *stub) Diff(ctx context.Context, from, to string) iter.Seq2[primitives.DiffEntry, error] {
	s.rec("Diff", from+" "+to)
	return func(yield func(primitives.DiffEntry, error) bool) {
		yield(primitives.DiffEntry{Change: primitives.DiffAdded, Type: 'F', Path: "/x"}, nil)
	}
}

func (s *stub) PlanDestroySnapshots(dataset, spec string, recursive, deferred, clones bool) (*primitives.DestroyPlan, error) {
	return &primitives.DestroyPlan{Snapshots: []string{dataset + "@x"}, Reclaim: 5}, s.rec("PlanDestroySnapshots", dataset, "@", spec, fmt.Sprint(recursive, deferred, clones))
}

func (s *stub) DestroySnapshotSpec(dataset, spec string, recursive, deferred, clones bool) ([]string, error) {
	return []string{dataset + "@x"}, s.rec("DestroySnapshotSpec", dataset, "@", spec, fmt.Sprint(recursive, deferred, clones))
}

func (s *stub) PlanDestroy(name string, opts *primitives.DestroyOptions) (*primitives.DestroyPlan, error) {
	return &primitives.DestroyPlan{Order: []string{name}}, s.rec("PlanDestroy", name, fmt.Sprint(opts.Recursive, opts.Clones))
}

func (s *stub) SendSizes(snapshot string, o *primitives.SendOptions) ([]primitives.StreamSize, error) {
	return []primitives.StreamSize{{To: snapshot, Bytes: 4242}}, s.rec("SendSizes", snapshot)
}

func (s *stub) SendSize(snapshot string, o *primitives.SendOptions) (uint64, error) {
	s.sendOpts = o
	return 4242, s.rec("SendSize", snapshot)
}
func (s *stub) ResumeSend(ctx context.Context, w io.Writer, token string, o *primitives.SendOptions) error {
	s.sendOpts = o
	return s.rec("ResumeSend", token)
}
func (s *stub) Redact(snapshot, bookmark string, snaps []string) error {
	return s.rec("Redact", snapshot, " ", bookmark, " ", snaps)
}
func (s *stub) Receive(ctx context.Context, r io.Reader, target string, o *primitives.ReceiveOptions) ([]primitives.Received, error) {
	s.recvOpts = o
	return []primitives.Received{{Snapshot: target + "@got"}}, s.rec("Receive", target)
}
func (s *stub) ResumeToken(ds string) (string, bool, error) {
	return s.token, s.token != "", s.rec("ResumeToken", ds)
}
func (s *stub) AbortResume(ds string) error { return s.rec("AbortResume", ds) }

func (s *stub) Mounts() ([]primitives.Mount, error) {
	return []primitives.Mount{{Dataset: "tank/a", Mountpoint: "/tank/a"}}, s.rec("Mounts")
}
func (s *stub) IsMounted(ds string) (string, bool, error) {
	return "/" + ds, true, s.rec("IsMounted", ds)
}
func (s *stub) MountPoint(ds string) (string, error) { return "/" + ds, s.rec("MountPoint", ds) }
func (s *stub) Mount(ds string, o *primitives.MountOptions) error {
	s.mountOpts = o
	return s.rec("Mount", ds)
}
func (s *stub) Unmount(ds string, o *primitives.UnmountOptions) error {
	s.unmountOpts = o
	return s.rec("Unmount", ds)
}
func (s *stub) MountAll(ds string, o *primitives.MountOptions) error {
	s.mountOpts = o
	return s.rec("MountAll", ds)
}
func (s *stub) UnmountAll(ds string, o *primitives.UnmountOptions) error {
	s.unmountOpts = o
	return s.rec("UnmountAll", ds)
}
