package primitives

import (
	"errors"
	"sort"
	"strings"
)

// A changelist is libzfs's way to change something that mounted filesystems
// depend on (their name, mountpoint or canmount): the affected filesystems
// are unmounted first, the change is made, and then they are mounted again,
// at their new place if need be. See libzfs_changelist.c.
type changelist struct {
	c    *Client
	prop string // "name", "mountpoint" or "canmount"
	// force unmounts busy filesystems.
	force bool
	nodes []clNode // parents first
	// undone are the nodes prefix has unmounted so far.
	undone []int
}

type clNode struct {
	name    string
	where   string // where it was mounted; "" if it was not
	mounted bool
	self    bool // the dataset the change is made on
}

// gather collects dataset and the filesystems below it that the change
// reaches: for a name change all of them, for mountpoint and canmount those
// that do not set the property themselves. Only mounted ones are of
// interest, except that the dataset itself is always kept (canmount=on may
// have to mount it afterwards).
func (c *Client) gather(dataset, prop string, force bool) (*changelist, error) {
	cl := &changelist{c: c, prop: prop, force: force}
	if c.m == nil || strings.ContainsAny(dataset, "@#") {
		return cl, nil
	}
	root, err := c.get(dataset, false, nil)
	if err != nil {
		return nil, err
	}
	if root.Type != Filesystem {
		return cl, nil
	}
	mounted, err := c.mountedSet()
	if err != nil {
		return nil, err
	}
	err = c.Walk(dataset, ListOptions{Types: Filesystem, Depth: Unlimited}, func(d *Dataset) error {
		self := d.Name == dataset
		if !self && prop != "name" {
			if s, ok := d.Source(prop); ok && (s.Kind == SourceLocal || s.Kind == SourceReceived) {
				return nil
			}
		}
		where, isMounted := mounted[d.Name]
		if self || isMounted {
			cl.nodes = append(cl.nodes, clNode{name: d.Name, where: where, mounted: isMounted, self: self})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(cl.nodes, func(i, j int) bool { return cl.nodes[i].name < cl.nodes[j].name })
	return cl, nil
}

// prefix unmounts the mounted nodes, children first. If one cannot be
// unmounted the ones already done are mounted again and the error returned.
func (cl *changelist) prefix() error {
	for i := len(cl.nodes) - 1; i >= 0; i-- {
		n := cl.nodes[i]
		if !n.mounted {
			continue
		}
		if err := cl.c.m.unmount(n.where, cl.force); err != nil {
			restoreErr := cl.restore()
			return firstThen(&MountError{Op: "unmount", Dataset: n.name, Mountpoint: n.where, Err: err}, restoreErr)
		}
		cl.undone = append(cl.undone, i)
	}
	return nil
}

// restore mounts again what prefix unmounted, parents first.
func (cl *changelist) restore() error {
	idx := append([]int(nil), cl.undone...)
	sort.Ints(idx)
	cl.undone = nil
	var errs []error
	for _, i := range idx {
		n := cl.nodes[i]
		d, err := cl.c.get(n.name, false, nil)
		if err != nil {
			continue
		}
		if err := cl.c.mountAt(d, n.where, &MountOptions{Overlay: true}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// renamed tells the list that old and everything below it now has the name
// new.
func (cl *changelist) renamed(old, new string) {
	for i := range cl.nodes {
		n := &cl.nodes[i]
		if n.name == old || strings.HasPrefix(n.name, old+"/") {
			n.name = new + n.name[len(old):]
		}
	}
}

// postfix mounts the filesystems that were mounted before, parents first,
// at the place their properties now say. Failures do not stop it; they are
// returned together. After a mountpoint change the old directories are
// removed if empty, as libzfs does.
func (cl *changelist) postfix() error {
	var errs []error
	var stale []string // old mountpoints to remove once everything is mounted
	for _, n := range cl.nodes {
		d, err := cl.c.get(n.name, false, nil)
		if err != nil {
			continue // destroyed or not there any more
		}
		want := n.mounted
		if cl.prop == "canmount" && n.self {
			if cm, _ := d.Enum("canmount"); cm == "on" {
				want = true
			}
		}
		if ks, _ := d.Enum("keystatus"); ks == "unavailable" {
			continue
		}
		mp, err := cl.c.mountpointOf(d, false)
		if !want || err != nil {
			if n.mounted && cl.prop == "mountpoint" && n.where != "" && n.where != mp {
				stale = append(stale, n.where)
			}
			continue
		}
		if now, ok, _ := cl.c.IsMounted(n.name); ok && now == mp {
			continue
		}
		if err := cl.c.mountAt(d, mp, &MountOptions{}); err != nil {
			errs = append(errs, err)
			continue
		}
		if cl.prop == "mountpoint" && n.where != "" && n.where != mp {
			stale = append(stale, n.where)
		}
	}
	// Deepest first: a directory can only go once the ones inside it did.
	sort.Slice(stale, func(i, j int) bool { return len(stale[i]) > len(stale[j]) })
	for _, dir := range stale {
		cl.c.m.rmdir(dir) // best effort, fails if not empty
	}
	cl.undone = nil
	return errors.Join(errs...)
}

// firstThen returns err, with the error of the follow-up step joined to it
// only if that failed too.
func firstThen(err, then error) error {
	if then == nil {
		return err
	}
	if err == nil {
		return then
	}
	return errors.Join(err, then)
}
