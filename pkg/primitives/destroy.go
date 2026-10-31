package primitives

import (
	"errors"
	"fmt"
	"strings"
	"syscall"

	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// DestroyOptions are the settings of Destroy.
type DestroyOptions struct {
	// Recursive also destroys the snapshots and child datasets (zfs destroy
	// -r). Clones of snapshots outside the tree stop it, see Clones.
	Recursive bool
	// Clones also destroys the dependents outside the tree: the clones of
	// its snapshots, their snapshots and clones in turn (zfs destroy -R).
	// It implies Recursive.
	Clones bool
	// Defer marks a snapshot that is in use for destruction when the last
	// hold or clone goes away. It applies to snapshots only.
	Defer bool
	// Force unmounts busy filesystems first (zfs destroy -f). Mounted
	// filesystems are always unmounted before they are destroyed.
	Force bool
	// DryRun makes Destroy check and plan only (zfs destroy -n). See
	// PlanDestroy for the plan.
	DryRun bool
}

// DependentsError is returned, before anything is destroyed, when a dataset
// has dependents the options do not cover: children (and snapshots) without
// Recursive, or clones of its snapshots without Clones. It matches EEXIST
// for the first and EBUSY for the second, the errors the kernel gives.
type DependentsError struct {
	Target string
	// Children are the descendants and snapshots that need Recursive.
	Children []string
	// Clones are the dependents outside the tree that need Clones.
	Clones []string
}

func (e *DependentsError) Error() string {
	if len(e.Children) > 0 {
		return fmt.Sprintf("zfs: cannot destroy %q: dataset has children; use Recursive to destroy the following datasets: %s", e.Target, strings.Join(e.Children, ", "))
	}
	return fmt.Sprintf("zfs: cannot destroy %q: dataset has dependent clones; use Clones to destroy the following datasets: %s", e.Target, strings.Join(e.Clones, ", "))
}

func (e *DependentsError) Is(target error) bool {
	if len(e.Children) > 0 {
		return errors.Is(syscall.EEXIST, target)
	}
	return errors.Is(syscall.EBUSY, target)
}

// dependents lists what depends on root, in the order libzfs destroys it
// (iter_dependents_cb): for a filesystem its child datasets, then its
// snapshots; for a snapshot its clones first. Everything is listed after what
// depends on it, and root itself is not.
func (c *Client) dependents(root *Dataset, allowRecursion bool) ([]*Dataset, error) {
	var out []*Dataset
	var stack []uint64
	var visit func(d *Dataset, first bool) error
	visit = func(d *Dataset, first bool) error {
		switch d.Type {
		case Snapshot:
			for _, n := range d.Clones() {
				cd, err := c.get(n, false, nil)
				if isNotExist(err) {
					continue
				}
				if err != nil {
					return err
				}
				if err := visit(cd, false); err != nil {
					return err
				}
			}
		case Filesystem, Volume:
			for _, g := range stack {
				if g == d.GUID {
					if allowRecursion {
						return nil
					}
					return fmt.Errorf("zfs: cannot determine dependent datasets: recursive dependency at %q", d.Name)
				}
			}
			stack = append(stack, d.GUID)
			defer func() { stack = stack[:len(stack)-1] }()
			var kids []*Dataset
			err := c.Walk(d.Name, ListOptions{Types: Filesystem | Volume, Depth: 1}, func(k *Dataset) error {
				if k.Name != d.Name {
					kids = append(kids, k)
				}
				return nil
			})
			if err != nil {
				return err
			}
			for _, k := range kids {
				if err := visit(k, false); err != nil {
					return err
				}
			}
			snaps, err := c.snapshotsOf(d.Name)
			if err != nil {
				return err
			}
			for _, sn := range snaps {
				if err := visit(sn, false); err != nil {
					return err
				}
			}
		}
		if !first {
			out = append(out, d)
		}
		return nil
	}
	if err := visit(root, true); err != nil {
		return nil, err
	}
	return out, nil
}

// destroyItem is one step of a destroy.
type destroyItem struct {
	d      *Dataset
	inside bool // in the tree of the target; the others are clones
}

func (c *Client) planDestroy(name string, o *DestroyOptions) (*DestroyPlan, []destroyItem, error) {
	if o == nil {
		o = &DestroyOptions{}
	}
	if name == "" || strings.ContainsAny(name, "@#") {
		return nil, nil, fmt.Errorf("zfs: destroy: %q is not a filesystem or volume (snapshots go through DestroySnapshotSpec)", name)
	}
	if o.Defer {
		return nil, nil, fmt.Errorf("zfs: cannot destroy %q: only snapshots can be destroyed deferred: %w", name, syscall.EINVAL)
	}
	recursive := o.Recursive || o.Clones
	isPool := !strings.Contains(name, "/")
	if isPool && !recursive {
		return nil, nil, fmt.Errorf("zfs: cannot destroy %q: operation does not apply to pools; use Recursive to destroy all datasets in the pool, or destroy the pool itself", name)
	}
	target, err := c.get(name, false, nil)
	if err != nil {
		return nil, nil, err
	}
	if target.Type != Filesystem && target.Type != Volume {
		return nil, nil, fmt.Errorf("zfs: destroy: %q is not a filesystem or volume", name)
	}
	deps, err := c.dependents(target, true)
	if err != nil {
		return nil, nil, err
	}
	var items []destroyItem
	derr := &DependentsError{Target: name}
	for _, d := range deps {
		inside := strings.HasPrefix(d.Name, name+"/") || strings.HasPrefix(d.Name, name+"@")
		switch {
		case o.Clones:
		case inside && !recursive:
			derr.Children = append(derr.Children, d.Name)
		case !inside && recursive:
			derr.Clones = append(derr.Clones, d.Name)
		}
		items = append(items, destroyItem{d, inside})
	}
	if len(derr.Children) > 0 || len(derr.Clones) > 0 {
		return nil, nil, derr
	}
	if !isPool { // the root dataset of a pool is not destroyed
		items = append(items, destroyItem{target, true})
	}
	plan := &DestroyPlan{}
	for _, it := range items {
		plan.Order = append(plan.Order, it.d.Name)
		if it.d.Type == Snapshot {
			plan.Snapshots = append(plan.Snapshots, it.d.Name)
		} else {
			plan.Datasets = append(plan.Datasets, it.d.Name)
		}
	}
	return plan, items, nil
}

// PlanDestroy works out what Destroy of a filesystem or volume would destroy,
// in the order it happens, without changing anything (zfs destroy -nv). It
// fails with a *DependentsError where Destroy would.
func (c *Client) PlanDestroy(name string, o *DestroyOptions) (*DestroyPlan, error) {
	plan, _, err := c.planDestroy(name, o)
	return plan, err
}

// Destroy destroys a filesystem, volume, snapshot or bookmark, depending on
// the name ("ds", "ds@snap", "ds#bookmark"), as zfs destroy does. A dataset
// with children or snapshots needs Recursive, one whose snapshots have
// clones elsewhere needs Clones; without them the call fails with a
// *DependentsError naming what is in the way, before anything is changed.
// Datasets are unmounted first (a busy mount stops the destroy unless Force),
// snapshots are destroyed in batches and always before their dataset, a clone
// before its origin, and the empty mountpoint directory of a filesystem with
// a default or inherited mountpoint is removed. Under Recursive an error does
// not stop the rest; the errors are returned together as a *BatchError.
func (c *Client) Destroy(name string, opts *DestroyOptions) error {
	if opts == nil {
		opts = &DestroyOptions{}
	}
	switch {
	case strings.Contains(name, "@"):
		ds, spec, _ := strings.Cut(name, "@")
		if opts.DryRun {
			_, err := c.PlanDestroySnapshots(ds, spec, opts.Recursive || opts.Clones, opts.Defer, opts.Clones)
			return err
		}
		if opts.Clones || opts.Recursive || strings.ContainsAny(spec, "%,") {
			_, err := c.DestroySnapshotSpec(ds, spec, opts.Recursive || opts.Clones, opts.Defer, opts.Clones)
			return err
		}
		return c.DestroySnapshots([]string{name}, opts.Defer)
	case strings.Contains(name, "#"):
		switch {
		case opts.DryRun:
			return errors.New("zfs: destroy: a dry run is not supported with a bookmark")
		case opts.Defer:
			return errors.New("zfs: destroy: Defer is not supported with a bookmark")
		case opts.Recursive || opts.Clones:
			return errors.New("zfs: destroy: Recursive is not supported with a bookmark")
		}
		return c.DestroyBookmarks([]string{name})
	}
	_, items, err := c.planDestroy(name, opts)
	if err != nil || opts.DryRun {
		return err
	}
	cl, err := c.gather(name, "name", opts.Force)
	if err != nil {
		return err
	}
	if err := cl.prefix(); err != nil {
		return err
	}
	if err := c.destroyItems(name, items, opts); err != nil {
		cl.postfix() // put back what is still there
		return err
	}
	return nil
}

// destroyItems carries out a plan.
func (c *Client) destroyItems(root string, items []destroyItem, opts *DestroyOptions) error {
	recursive := opts.Recursive || opts.Clones
	errs := map[string]error{}
	fail := func(name string, err error) error {
		if !recursive {
			return err
		}
		errs[name] = err // go on with the rest, as zfs destroy -r does
		return nil
	}
	var batch []string
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := c.DestroySnapshots(batch, false)
		batch = nil
		return err
	}
	for _, it := range items {
		name := it.d.Name
		if it.d.Type == Snapshot {
			batch = append(batch, name)
			continue
		}
		if err := flush(); err != nil {
			if err := fail(name, err); err != nil {
				return err
			}
			continue
		}
		var mountpoint string
		if it.d.Type == Filesystem {
			if !it.inside && c.m != nil {
				// The clones outside the tree were not unmounted with it.
				if err := c.Unmount(name, &UnmountOptions{Force: opts.Force}); err != nil {
					if err := fail(name, err); err != nil {
						return err
					}
					continue
				}
			}
			if s, ok := it.d.Source("mountpoint"); !ok || s.Kind == SourceDefault || s.Kind == SourceInherited {
				if mp, err := c.mountpointOf(it.d, true); err == nil {
					mountpoint = mp
				}
			}
		}
		if err := c.destroyHead(name); err != nil {
			if err := fail(name, err); err != nil {
				return err
			}
			continue
		}
		if mountpoint != "" && c.m != nil {
			c.m.rmdir(mountpoint) // best effort: libzfs ignores a failure too
		}
	}
	if err := flush(); err != nil {
		if err := fail(root, err); err != nil {
			return err
		}
	}
	if len(errs) > 0 {
		return &BatchError{Op: "destroy", Errors: errs}
	}
	return nil
}

func (c *Client) destroyHead(name string) error {
	cmd := ioctl.NewCmd()
	if err := cmd.SetName(name); err != nil {
		return err
	}
	return c.c.Ioctl(ioctl.IOCDestroy, cmd)
}
