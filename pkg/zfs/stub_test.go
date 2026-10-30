package zfs

import (
	"fmt"
	"sort"

	"github.com/go-freebsd/zfs/pkg/primitives"
)

// stub is a backend that records what it is asked to do.
type stub struct {
	calls []string // "Method arg arg ..."

	// recorded arguments of the last call of each kind
	createOpts *primitives.CreateOptions
	setProps   map[string]any
	snapNames  []string
	snapProps  map[string]any
	destroyOpt *primitives.DestroyOptions
	key        *primitives.Key

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
