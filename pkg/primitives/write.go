package primitives

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"syscall"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// BatchError reports the items of a batch call (snapshot, hold, release,
// bookmark, destroy) that the kernel refused, each with its errno. The
// kernel applies such a batch all or nothing.
type BatchError struct {
	Op     string
	Errors map[string]error
	Err    error // the error of the call itself
}

func (e *BatchError) Error() string {
	names := make([]string, 0, len(e.Errors))
	for n := range e.Errors {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s: %v", n, e.Errors[n]))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("zfs: %s: %v", e.Op, e.Err)
	}
	return fmt.Sprintf("zfs: %s: %s", e.Op, strings.Join(parts, "; "))
}

// Unwrap returns the call's error and every item's errno, so errors.Is
// works (for example with fs.ErrExist).
func (e *BatchError) Unwrap() []error {
	errs := []error{}
	if e.Err != nil {
		errs = append(errs, e.Err)
	}
	names := make([]string, 0, len(e.Errors))
	for n := range e.Errors {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		errs = append(errs, e.Errors[n])
	}
	return errs
}

// batchError turns the per-item error list the kernel returns with a failed
// batch call into a *BatchError. Other errors pass through.
func batchError(op string, err error) error {
	var ie *ioctl.Error
	if err == nil || !errors.As(err, &ie) || ie.Detail == nil {
		return err
	}
	m := map[string]error{}
	for _, p := range ie.Detail.Pairs() {
		if n, ok := p.Value.(int32); ok {
			m[p.Name] = syscall.Errno(n)
		}
	}
	if len(m) == 0 {
		return err
	}
	return &BatchError{Op: op, Errors: m, Err: ie.Errno}
}

// poolOf returns the pool of a dataset, snapshot or bookmark name.
func poolOf(name string) string {
	if i := strings.IndexAny(name, "/@#"); i >= 0 {
		return name[:i]
	}
	return name
}

// samePool returns the pool of names, which must all be in one.
func samePool(op string, names []string) (string, error) {
	if len(names) == 0 {
		return "", fmt.Errorf("zfs: %s: no names given", op)
	}
	pool := poolOf(names[0])
	for _, n := range names {
		if poolOf(n) != pool {
			return "", fmt.Errorf("zfs: %s: %s and %s are in different pools", op, names[0], n)
		}
	}
	return pool, nil
}

// CreateOptions are the optional settings of Create and CreateVolume.
type CreateOptions struct {
	// Props are the initial properties, in the types SetProps takes. The
	// create-only ones (encryption, casesensitivity, volblocksize,
	// utf8only, ...) are allowed here.
	Props map[string]any

	// Key is the key of an encrypted dataset. Set Props["encryption"] to
	// pick the algorithm ("on" means aes-256-gcm); with a Key and no
	// encryption property "on" is used. A dataset below an encrypted parent
	// needs no Key: it inherits the parent's encryption root.
	Key *Key

	// Parents creates missing parent filesystems first, as zfs create -p.
	Parents bool

	// NoMount leaves a new filesystem unmounted. Without it a filesystem
	// that is mountable and has canmount=on is mounted after it is created,
	// as zfs create does (this needs FreeBSD and root).
	NoMount bool
}

// props builds the property list of a create or clone call.
func (o *CreateOptions) props(extra map[string]any) (*nvlist.List, []byte, error) {
	merged := map[string]any{}
	if o != nil {
		for k, v := range o.Props {
			merged[k] = v
		}
	}
	for k, v := range extra {
		merged[k] = v
	}
	in := nvlist.New()
	var wkey []byte
	if o != nil && o.Key != nil {
		if _, ok := merged["encryption"]; !ok {
			merged["encryption"] = "on"
		}
		kp, w, err := newKeyProps(o.Key)
		if err != nil {
			return nil, nil, err
		}
		wkey = w
		for _, p := range kp.Pairs() {
			if _, set := merged[p.Name]; set {
				return nil, nil, propErr(p.Name, ErrBadValue, "comes from the Key, do not set it")
			}
			if err := in.Add(p.Name, p.Value); err != nil {
				return nil, nil, err
			}
		}
	}
	names := make([]string, 0, len(merged))
	for n := range merged {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if in.Has(name) {
			continue
		}
		v, err := convertPropFor(name, merged[name], false, true)
		if err != nil {
			return nil, nil, err
		}
		if err := in.Add(name, v); err != nil {
			return nil, nil, propErr(name, ErrBadValue, "%v", err)
		}
	}
	return in, wkey, nil
}

// Create creates a filesystem and mounts it (see CreateOptions.NoMount).
func (c *Client) Create(name string, opts *CreateOptions) error {
	return c.create(name, ostZFS, opts, nil)
}

// CreateVolume creates a volume of size bytes (a multiple of volblocksize).
func (c *Client) CreateVolume(name string, size uint64, opts *CreateOptions) error {
	return c.create(name, ostZVOL, opts, map[string]any{"volsize": size})
}

func (c *Client) create(name string, typ int32, opts *CreateOptions, extra map[string]any) error {
	if err := checkDatasetName(name); err != nil {
		return err
	}
	props, wkey, err := opts.props(extra)
	if err != nil {
		return err
	}
	var made []string
	if opts != nil && opts.Parents {
		var err error
		if made, err = c.createParents(name); err != nil {
			return err
		}
	}
	in := nvlist.New()
	in.Add("type", typ)
	if props.Len() > 0 {
		in.Add("props", props)
	}
	if wkey != nil {
		in.Add("hidden_args", hiddenArgs(wkey))
	}
	if _, err = c.c.Call(ioctl.IOCCreate, name, in); err != nil {
		return err
	}
	if typ != ostZFS || (opts != nil && opts.NoMount) {
		return nil
	}
	return c.autoMount(append(made, name)...)
}

// autoMount mounts the named filesystems in order if they are mountable and
// have canmount=on (what zfs create, clone and receive do after the kernel
// made the dataset). It does nothing without a mounter.
func (c *Client) autoMount(names ...string) error {
	if c.m == nil {
		return nil
	}
	var errs []error
	for _, n := range names {
		d, err := c.get(n, false, nil)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if cm, _ := d.Enum("canmount"); cm != "on" {
			continue
		}
		if ks, _ := d.Enum("keystatus"); ks == "unavailable" {
			continue // an encrypted dataset without its key waits (a raw receive)
		}
		mp, err := c.mountpointOf(d, false)
		if err != nil {
			continue // mountpoint none, legacy, jailed, ...
		}
		if _, ok, _ := c.IsMounted(n); ok {
			continue
		}
		if err := c.mountAt(d, mp, &MountOptions{}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// createParents creates every missing ancestor of name below the pool,
// without mounting them, and returns their names.
func (c *Client) createParents(name string) ([]string, error) {
	var made []string
	parts := strings.Split(name, "/")
	for i := 2; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		ok, err := c.Exists(parent)
		if err != nil {
			return made, err
		}
		if !ok {
			if err := c.create(parent, ostZFS, &CreateOptions{NoMount: true}, nil); err != nil {
				return made, err
			}
			made = append(made, parent)
		}
	}
	return made, nil
}

func checkDatasetName(name string) error {
	if name == "" || strings.ContainsAny(name, "@#") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return fmt.Errorf("zfs: invalid dataset name %q", name)
	}
	return nil
}

// Snapshot creates snapshots atomically: all of them or none, at the same
// transaction. The names are "dataset@snap" and must be in one pool. props
// may hold user properties.
func (c *Client) Snapshot(names []string, props map[string]any) error {
	pool, err := samePool("snapshot", names)
	if err != nil {
		return err
	}
	snaps := nvlist.New()
	owner := map[string]string{}
	for _, n := range names {
		ds, _, found := strings.Cut(n, "@")
		if !found {
			return fmt.Errorf("zfs: snapshot: %q is not a snapshot name", n)
		}
		// The kernel refuses a second snapshot of one dataset with EXDEV.
		if prev, dup := owner[ds]; dup {
			return fmt.Errorf("zfs: snapshot: %s and %s are of the same dataset, one call takes one snapshot per dataset: %w", prev, n, syscall.EXDEV)
		}
		owner[ds] = n
		snaps.Add(n, nvlist.Flag{})
	}
	in := nvlist.New()
	in.Add("snaps", snaps)
	if len(props) > 0 {
		pl := nvlist.New()
		for _, k := range sortedKeys(props) {
			v, err := convertProp(k, props[k], true)
			if err != nil {
				return err
			}
			pl.Add(k, v)
		}
		in.Add("props", pl)
	}
	_, err = c.c.Call(ioctl.IOCSnapshot, pool, in)
	return batchError("snapshot", err)
}

func sortedKeys(m map[string]any) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// Clone creates a filesystem or volume from a snapshot.
func (c *Client) Clone(name, origin string, opts *CreateOptions) error {
	if err := checkDatasetName(name); err != nil {
		return err
	}
	if !strings.Contains(origin, "@") {
		return fmt.Errorf("zfs: clone: origin %q is not a snapshot", origin)
	}
	if opts != nil && opts.Key != nil {
		return fmt.Errorf("zfs: clone: %w: a clone uses the key of its origin", ErrBadKey)
	}
	props, _, err := opts.props(nil)
	if err != nil {
		return err
	}
	var made []string
	if opts != nil && opts.Parents {
		var err error
		if made, err = c.createParents(name); err != nil {
			return err
		}
	}
	in := nvlist.New()
	in.Add("origin", origin)
	if props.Len() > 0 {
		in.Add("props", props)
	}
	if _, err = c.c.Call(ioctl.IOCClone, name, in); err != nil {
		return err
	}
	if opts != nil && opts.NoMount {
		return nil
	}
	if d, err := c.get(name, true, nil); err == nil && d.Type != Filesystem {
		return c.autoMount(made...)
	}
	return c.autoMount(append(made, name)...)
}

// DestroyOptions are the settings of Destroy.
type DestroyOptions struct {
	// Recursive also destroys the snapshots and child datasets.
	Recursive bool
	// Defer marks a snapshot that is in use for destruction when the last
	// hold or clone goes away.
	Defer bool
	// Force unmounts busy filesystems first (zfs destroy -f). Mounted
	// filesystems are always unmounted before they are destroyed.
	Force bool
}

// Destroy destroys a filesystem, volume, snapshot or bookmark, depending on
// the name ("ds", "ds@snap", "ds#bookmark"). Without opts.Recursive a
// dataset with snapshots or children fails with EEXIST or EBUSY.
func (c *Client) Destroy(name string, opts *DestroyOptions) error {
	if opts == nil {
		opts = &DestroyOptions{}
	}
	switch {
	case strings.Contains(name, "@"):
		return c.DestroySnapshots([]string{name}, opts.Defer)
	case strings.Contains(name, "#"):
		return c.DestroyBookmarks([]string{name})
	}
	cl, err := c.gather(name, "name", opts.Force)
	if err != nil {
		return err
	}
	if err := cl.prefix(); err != nil {
		return err
	}
	if err := c.destroyTree(name, opts); err != nil {
		cl.postfix() // put back what is still there
		return err
	}
	return nil
}

func (c *Client) destroyTree(name string, opts *DestroyOptions) error {
	if opts.Recursive {
		var snaps, sets []string
		err := c.Walk(name, ListOptions{Types: Filesystem | Volume | Snapshot, Depth: Unlimited}, func(d *Dataset) error {
			if d.Type == Snapshot {
				snaps = append(snaps, d.Name)
			} else {
				sets = append(sets, d.Name)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(snaps) > 0 {
			if err := c.DestroySnapshots(snaps, false); err != nil {
				return err
			}
		}
		for i := len(sets) - 1; i >= 0; i-- { // children before parents
			if err := c.destroyHead(sets[i]); err != nil {
				return err
			}
		}
		return nil
	}
	return c.destroyHead(name)
}

func (c *Client) destroyHead(name string) error {
	cmd := ioctl.NewCmd()
	if err := cmd.SetName(name); err != nil {
		return err
	}
	return c.c.Ioctl(ioctl.IOCDestroy, cmd)
}

// DestroySnapshots destroys snapshots, all or none. With deferred, a
// snapshot with holds or clones is only marked and goes away later.
func (c *Client) DestroySnapshots(names []string, deferred bool) error {
	pool, err := samePool("destroy snapshots", names)
	if err != nil {
		return err
	}
	snaps := nvlist.New()
	for _, n := range names {
		if !strings.Contains(n, "@") {
			return fmt.Errorf("zfs: destroy snapshots: %q is not a snapshot name", n)
		}
		snaps.Add(n, nvlist.Flag{})
	}
	in := nvlist.New()
	in.Add("snaps", snaps)
	if deferred {
		in.Add("defer", nvlist.Flag{})
	}
	_, err = c.c.Call(ioctl.IOCDestroySnaps, pool, in)
	return batchError("destroy snapshots", err)
}

// Rename renames a filesystem, volume or snapshot. With recursive, a snapshot
// of that name is renamed in every descendant too. The new name of a
// filesystem may be below another parent in the same pool.
func (c *Client) Rename(oldName, newName string, recursive bool) error {
	cl, err := c.gather(oldName, "name", false)
	if err != nil {
		return err
	}
	if err := cl.prefix(); err != nil {
		return err
	}
	err = c.renameIoctl(oldName, newName, recursive)
	if err == nil {
		cl.renamed(oldName, newName)
	}
	return firstThen(err, cl.postfix())
}

func (c *Client) renameIoctl(oldName, newName string, recursive bool) error {
	cmd := ioctl.NewCmd()
	if err := cmd.SetName(oldName); err != nil {
		return err
	}
	if err := cmd.SetValue(newName); err != nil {
		return err
	}
	if recursive {
		cmd.Cookie = 1
	}
	return c.c.Ioctl(ioctl.IOCRename, cmd)
}

// Rollback returns a filesystem or volume to a snapshot and returns the
// name of the snapshot used. With an empty snapshot it is the latest one.
// A snapshot that is not the latest fails with EEXIST, as newer snapshots
// would be lost: destroy them first.
func (c *Client) Rollback(dataset, snapshot string) (string, error) {
	var in *nvlist.List
	if snapshot != "" {
		if !strings.Contains(snapshot, "@") {
			snapshot = dataset + "@" + snapshot
		}
		in = nvlist.New()
		in.Add("target", snapshot)
	}
	out, err := c.c.Call(ioctl.IOCRollback, dataset, in)
	if err != nil {
		return "", err
	}
	if out != nil {
		if s, ok := out.String("target"); ok {
			return s, nil
		}
	}
	return snapshot, nil
}

// Promote swaps a clone with the snapshot it came from. If a snapshot of
// the same name already exists in the origin's lineage, the error is a
// *PromoteConflictError.
func (c *Client) Promote(clone string) error {
	cmd := ioctl.NewCmd()
	if err := cmd.SetName(clone); err != nil {
		return err
	}
	err := c.c.Ioctl(ioctl.IOCPromote, cmd)
	if errors.Is(err, syscall.EEXIST) {
		return &PromoteConflictError{Clone: clone, Snapshot: cmd.GetString(), Err: err}
	}
	return err
}

// PromoteConflictError says which snapshot name blocks a promotion.
type PromoteConflictError struct {
	Clone, Snapshot string
	Err             error
}

func (e *PromoteConflictError) Error() string {
	return fmt.Sprintf("zfs: cannot promote %s: snapshot %q already exists in the origin", e.Clone, e.Snapshot)
}
func (e *PromoteConflictError) Unwrap() error { return e.Err }

// Hold puts a hold named tag on each snapshot, so that it cannot be
// destroyed until the hold is released.
func (c *Client) Hold(tag string, snapshots ...string) error {
	pool, err := samePool("hold", snapshots)
	if err != nil {
		return err
	}
	holds := nvlist.New()
	for _, s := range snapshots {
		holds.Add(s, tag)
	}
	in := nvlist.New()
	in.Add("holds", holds)
	_, err = c.c.Call(ioctl.IOCHold, pool, in)
	return batchError("hold", err)
}

// Release removes the hold named tag from each snapshot.
func (c *Client) Release(tag string, snapshots ...string) error {
	pool, err := samePool("release", snapshots)
	if err != nil {
		return err
	}
	in := nvlist.New()
	for _, s := range snapshots {
		t := nvlist.New()
		t.Add(tag, nvlist.Flag{})
		in.Add(s, t)
	}
	_, err = c.c.Call(ioctl.IOCRelease, pool, in)
	return batchError("release", err)
}

// CreateBookmark creates bookmark "dataset#name" from a snapshot "dataset@snap"
// (or from another bookmark, where the kernel supports it).
func (c *Client) CreateBookmark(bookmark, source string) error {
	if !strings.Contains(bookmark, "#") {
		return fmt.Errorf("zfs: bookmark: %q is not a bookmark name", bookmark)
	}
	in := nvlist.New()
	in.Add(bookmark, source)
	_, err := c.c.Call(ioctl.IOCBookmark, poolOf(bookmark), in)
	return batchError("bookmark", err)
}

// DestroyBookmarks destroys bookmarks, all or none.
func (c *Client) DestroyBookmarks(names []string) error {
	pool, err := samePool("destroy bookmarks", names)
	if err != nil {
		return err
	}
	in := nvlist.New()
	for _, n := range names {
		if !strings.Contains(n, "#") {
			return fmt.Errorf("zfs: destroy bookmarks: %q is not a bookmark name", n)
		}
		in.Add(n, nvlist.Flag{})
	}
	_, err = c.c.Call(ioctl.IOCDestroyBookmarks, pool, in)
	return batchError("destroy bookmarks", err)
}
