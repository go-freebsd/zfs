package zfs

import (
	"context"
	"iter"
	"strings"

	"github.com/go-freebsd/zfs/pkg/primitives"
)

// Dataset is a handle to a filesystem or volume, or to a pool's root
// dataset. Making one does no I/O; every method is one round trip to the
// kernel (a few for the recursive ones).
type Dataset struct {
	c    *Client
	name string
}

// Name is the full name, "pool/path/to/dataset".
func (d *Dataset) Name() string { return d.name }

// String returns the name.
func (d *Dataset) String() string { return d.name }

// Pool is the name of the pool the dataset is in.
func (d *Dataset) Pool() string {
	pool, _, _ := strings.Cut(d.name, "/")
	return pool
}

// Parent returns the dataset one level up, or nil for a pool's root.
func (d *Dataset) Parent() *Dataset {
	i := strings.LastIndexByte(d.name, '/')
	if i < 0 {
		return nil
	}
	return d.c.Dataset(d.name[:i])
}

// Child returns the handle of a child. name may have several components.
func (d *Dataset) Child(name string) *Dataset { return d.c.Dataset(d.name + "/" + name) }

// Exists reports whether the dataset exists.
func (d *Dataset) Exists() (bool, error) {
	ok, err := d.c.b.Exists(d.name)
	return ok, wrap("exists", d.name, err)
}

// Info reads the dataset's properties and state.
func (d *Dataset) Info() (*Info, error) {
	raw, err := d.c.b.Get(d.name)
	if err != nil {
		return nil, wrap("get", d.name, err)
	}
	return newInfo(d.c, raw), nil
}

// CreateFilesystem creates a filesystem below d and returns its handle.
// name is relative to d and may have several components with Parents().
// A filesystem that is mountable (see MountPoint) is mounted, unless
// NoMount is given; if only the mount fails the error is a mount error and
// the filesystem exists.
func (d *Dataset) CreateFilesystem(name string, opts ...Option) (*Dataset, error) {
	cfg, err := collect("create", scCreate, opts)
	if err != nil {
		return nil, wrap("create", d.name+"/"+name, err)
	}
	child := d.Child(name)
	if err := d.c.b.Create(child.name, cfg.create()); err != nil {
		return nil, wrap("create", child.name, err)
	}
	return child, nil
}

// CreateVolume creates a volume of size bytes below d.
func (d *Dataset) CreateVolume(name string, size uint64, opts ...Option) (*Dataset, error) {
	cfg, err := collect("create volume", scCreate, opts)
	if err != nil {
		return nil, wrap("create volume", d.name+"/"+name, err)
	}
	child := d.Child(name)
	if err := d.c.b.CreateVolume(child.name, size, cfg.create()); err != nil {
		return nil, wrap("create volume", child.name, err)
	}
	return child, nil
}

// Set changes properties. The options are the property options of Create.
func (d *Dataset) Set(opts ...Option) error {
	cfg, err := collect("set", scSet, opts)
	if err != nil {
		return wrap("set", d.name, err)
	}
	return wrap("set", d.name, d.c.b.SetProps(d.name, cfg.props))
}

// Inherit removes the local value of properties, so that they take the
// value of the parent again.
func (d *Dataset) Inherit(props ...string) error {
	for _, p := range props {
		if err := d.c.b.InheritProp(d.name, p); err != nil {
			return wrap("inherit "+p, d.name, err)
		}
	}
	return nil
}

// Snapshot creates the snapshot d@name. Only WithProp options (user
// properties) apply.
func (d *Dataset) Snapshot(name string, opts ...Option) (*Snapshot, error) {
	cfg, err := collect("snapshot", scSnapshot, opts)
	snap := d.c.Snapshot(d.name + "@" + name)
	if err != nil {
		return nil, wrap("snapshot", snap.name, err)
	}
	if err := d.c.b.Snapshot([]string{snap.name}, cfg.props); err != nil {
		return nil, wrap("snapshot", snap.name, err)
	}
	return snap, nil
}

// SnapshotAll snapshots d and all its descendant filesystems and volumes
// as one atomic operation: every snapshot is taken at the same moment, or
// none is.
func (d *Dataset) SnapshotAll(ctx context.Context, name string, opts ...Option) ([]*Snapshot, error) {
	cfg, err := collect("snapshot", scSnapshotAll, opts)
	if err != nil {
		return nil, wrap("snapshot", d.name+"@"+name, err)
	}
	names := []string{d.name + "@" + name}
	for in, err := range d.Descendants(ctx) {
		if err != nil {
			return nil, err
		}
		names = append(names, in.Name+"@"+name)
	}
	if err := d.c.b.Snapshot(names, cfg.props); err != nil {
		return nil, wrap("snapshot", d.name+"@"+name, err)
	}
	snaps := make([]*Snapshot, len(names))
	for i, n := range names {
		snaps[i] = d.c.Snapshot(n)
	}
	return snaps, nil
}

// Snapshots lists the snapshots of d (not of its descendants), oldest first.
func (d *Dataset) Snapshots(ctx context.Context) iter.Seq2[*Info, error] {
	return d.c.walk(ctx, "list snapshots", d.name, primitives.ListOptions{Types: primitives.Snapshot}, false)
}

// Children lists the child filesystems and volumes, by name.
func (d *Dataset) Children(ctx context.Context, opts ...Option) iter.Seq2[*Info, error] {
	return d.list(ctx, "list children", append([]Option{Depth(1)}, opts...))
}

// Descendants lists all filesystems and volumes below d, parents before
// their children. d itself is not included.
func (d *Dataset) Descendants(ctx context.Context, opts ...Option) iter.Seq2[*Info, error] {
	return d.list(ctx, "list", opts)
}

func (d *Dataset) list(ctx context.Context, op string, opts []Option) iter.Seq2[*Info, error] {
	cfg, err := collect(op, scList, opts)
	if err != nil {
		return func(yield func(*Info, error) bool) { yield(nil, wrap(op, d.name, err)) }
	}
	kinds := cfg.types
	if kinds == 0 {
		kinds = KindFilesystem | KindVolume
	}
	return d.c.walk(ctx, op, d.name, primitives.ListOptions{Types: kinds & (KindFilesystem | KindVolume), Depth: cfg.depth}, true)
}

// Bookmarks lists the bookmarks of d.
func (d *Dataset) Bookmarks() ([]*Info, error) {
	bms, err := d.c.b.Bookmarks(d.name)
	if err != nil {
		return nil, wrap("list bookmarks", d.name, err)
	}
	out := make([]*Info, len(bms))
	for i, b := range bms {
		out[i] = newInfo(d.c, b)
	}
	return out, nil
}

// Destroy destroys the dataset. Without Recursive it fails if the dataset
// has snapshots or children.
func (d *Dataset) Destroy(opts ...Option) error {
	cfg, err := collect("destroy", scDestroyDataset, opts)
	if err != nil {
		return wrap("destroy", d.name, err)
	}
	return wrap("destroy", d.name, d.c.b.Destroy(d.name, &primitives.DestroyOptions{Recursive: cfg.recursive, Force: cfg.forceUnmount}))
}

// Rename renames the dataset. newName is the full new name, which may be
// below another parent in the same pool. It returns the handle of the
// renamed dataset.
func (d *Dataset) Rename(newName string) (*Dataset, error) {
	if err := d.c.b.Rename(d.name, newName, false); err != nil {
		return nil, wrap("rename", d.name, err)
	}
	return d.c.Dataset(newName), nil
}

// Mount mounts the filesystem at its mountpoint (zfs mount). A dataset that
// has no mountpoint to mount at fails with ErrNotMountable; one that is
// mounted fails with ErrBusy unless Remount is given.
func (d *Dataset) Mount(opts ...Option) error {
	cfg, err := collect("mount", scMount, opts)
	if err != nil {
		return wrap("mount", d.name, err)
	}
	return wrapMount("mount", d.name, d.c.b.Mount(d.name, cfg.mountOpts()))
}

// Unmount unmounts the filesystem. One that is not mounted is not an error.
func (d *Dataset) Unmount(opts ...Option) error {
	cfg, err := collect("unmount", scUnmount, opts)
	if err != nil {
		return wrap("unmount", d.name, err)
	}
	return wrap("unmount", d.name, d.c.b.Unmount(d.name, cfg.unmountOpts()))
}

// IsMounted reports whether the filesystem is mounted, and where.
func (d *Dataset) IsMounted() (where string, mounted bool, err error) {
	where, mounted, err = d.c.b.IsMounted(d.name)
	return where, mounted, wrap("ismounted", d.name, err)
}

// MountPoint is where the filesystem is, or would be, mounted. It fails
// with ErrNotMountable if the mountpoint is none or legacy, canmount is off,
// or the dataset is not a filesystem.
func (d *Dataset) MountPoint() (string, error) {
	p, err := d.c.b.MountPoint(d.name)
	return p, wrap("mountpoint", d.name, err)
}

// MountAll mounts the filesystems at and below d that are mountable and not
// mounted yet, parents first (zfs mount -a). Failures are joined; the rest
// is still mounted.
func (d *Dataset) MountAll(opts ...Option) error {
	cfg, err := collect("mount all", scMount, opts)
	if err != nil {
		return wrap("mount all", d.name, err)
	}
	return wrapMount("mount all", d.name, d.c.b.MountAll(d.name, cfg.mountOpts()))
}

// UnmountAll unmounts the filesystems at and below d, children first
// (zfs unmount -a).
func (d *Dataset) UnmountAll(opts ...Option) error {
	cfg, err := collect("unmount all", scUnmount, opts)
	if err != nil {
		return wrap("unmount all", d.name, err)
	}
	return wrap("unmount all", d.name, d.c.b.UnmountAll(d.name, cfg.unmountOpts()))
}

// Promote makes a clone independent of its origin: the origin snapshot and
// the snapshots before it move to the clone, and the former parent becomes
// the clone's dependent. A name clash is a *primitives.PromoteConflictError.
func (d *Dataset) Promote() error { return wrap("promote", d.name, d.c.b.Promote(d.name)) }

// LoadKey loads the key of an encrypted dataset so it can be mounted and
// read. A wrong key fails with ErrKey.
func (d *Dataset) LoadKey(key *Key) error {
	return wrapKey("load key", d.name, d.c.b.LoadKey(d.name, key))
}

// CheckKey tests a key without loading it.
func (d *Dataset) CheckKey(key *Key) error {
	return wrapKey("check key", d.name, d.c.b.CheckKey(d.name, key))
}

// UnloadKey removes the key from memory. The dataset must be unmounted.
func (d *Dataset) UnloadKey() error {
	return wrapKey("unload key", d.name, d.c.b.UnloadKey(d.name))
}

// ChangeKey re-wraps the dataset's encryption with a new key, making it an
// encryption root. Its key must be loaded.
func (d *Dataset) ChangeKey(key *Key) error {
	return wrapKey("change key", d.name, d.c.b.ChangeKey(d.name, key))
}

// InheritKey makes the dataset use the key of its parent's encryption root.
func (d *Dataset) InheritKey() error {
	return wrapKey("inherit key", d.name, d.c.b.InheritKey(d.name))
}
