package primitives

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"syscall"
)

// Mounting is done in userland, as libzfs does: the kernel only creates
// datasets. The functions here follow zfs_mount, zfs_unmount and the
// changelist code of libzfs.

// Mount is one mounted ZFS filesystem.
type Mount struct {
	Dataset    string
	Mountpoint string
}

// mounter is what mounting needs from the operating system. The real one
// calls nmount(2) and friends; tests replace it.
type mounter interface {
	// nmount mounts the dataset at path with the given boolean options.
	nmount(dataset, path string, opts []string) error
	unmount(path string, force bool) error
	mounts() ([]Mount, error)
	mkdirAll(path string) error
	isEmptyDir(path string) (bool, error)
	rmdir(path string) error
	jailed() bool
}

var (
	// ErrNotMountable is matched by *NotMountableError.
	ErrNotMountable = errors.New("zfs: dataset is not mountable")
	// ErrMountpointNotEmpty is returned when the mountpoint directory has
	// content and overlay mounts are off.
	ErrMountpointNotEmpty = errors.New("zfs: mountpoint directory is not empty")
	// ErrKeyNotLoaded is returned for an encrypted dataset whose key is not
	// loaded and for which no key was given.
	ErrKeyNotLoaded = errors.New("zfs: encryption key is not loaded")
)

// NotMountableError says why a dataset is not mounted automatically.
type NotMountableError struct {
	Dataset string
	Reason  string
}

func (e *NotMountableError) Error() string {
	return fmt.Sprintf("zfs: %s is not mountable: %s", e.Dataset, e.Reason)
}
func (e *NotMountableError) Is(target error) bool { return target == ErrNotMountable }

// MountError is the error of a mount or unmount; Err unwraps to the errno or
// one of the sentinel errors.
type MountError struct {
	Op         string // "mount" or "unmount"
	Dataset    string
	Mountpoint string
	Err        error
}

func (e *MountError) Error() string {
	return fmt.Sprintf("zfs: %s %s at %s: %v", e.Op, e.Dataset, e.Mountpoint, e.Err)
}
func (e *MountError) Unwrap() error { return e.Err }

// MountOptions are the settings of Mount.
type MountOptions struct {
	// Options replaces the default mount options ("defaults"); the options
	// the properties imply (atime, exec, readonly, setuid, ...) are added.
	Options []string
	// Overlay allows mounting over a directory that has content (-O);
	// the dataset property overlay=on has the same effect.
	Overlay bool
	// Force mounts a redacted dataset too.
	Force bool
	// Remount updates an existing mount instead of failing (-o remount).
	Remount bool
	// Key is loaded first if the dataset is encrypted and its key is not
	// available. Without it such a dataset fails with ErrKeyNotLoaded.
	Key *Key
}

// UnmountOptions are the settings of Unmount.
type UnmountOptions struct {
	// Force unmounts a busy filesystem (-f).
	Force bool
	// UnloadKey also unloads the key of an encryption root (-u).
	UnloadKey bool
}

// Mounts lists the mounted ZFS filesystems, sorted by mountpoint.
func (c *Client) Mounts() ([]Mount, error) {
	if c.m == nil {
		return nil, errors.ErrUnsupported
	}
	ms, err := c.m.mounts()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Mountpoint < ms[j].Mountpoint })
	return ms, nil
}

// IsMounted reports whether the filesystem is mounted and where.
func (c *Client) IsMounted(dataset string) (where string, ok bool, err error) {
	ms, err := c.Mounts()
	if err != nil {
		return "", false, err
	}
	for _, m := range ms {
		if m.Dataset == dataset {
			return m.Mountpoint, true, nil
		}
	}
	return "", false, nil
}

// defaultMountpoint is the mountpoint a dataset has when nothing is set.
func defaultMountpoint(name string) string { return "/" + name }

// mountpointOf returns where the dataset would be mounted, or a
// *NotMountableError. d must carry all its properties.
func (c *Client) mountpointOf(d *Dataset, force bool) (string, error) {
	no := func(why string) (string, error) {
		return "", &NotMountableError{Dataset: d.Name, Reason: why}
	}
	if d.Type != Filesystem {
		return no("not a filesystem")
	}
	mp := d.Mountpoint()
	if _, ok := d.Props["mountpoint"]; !ok {
		mp = defaultMountpoint(d.Name)
	}
	switch mp {
	case "none":
		return no("mountpoint is none")
	case "legacy":
		return no("mountpoint is legacy")
	}
	if cm, _ := d.Enum("canmount"); cm == "off" {
		return no("canmount is off")
	}
	if j, _ := d.Bool("jailed"); j && c.m != nil && !c.m.jailed() {
		return no("jailed dataset outside a jail")
	}
	if n, _ := d.Uint64("redacted"); n != 0 && !force {
		return no("redacted dataset")
	}
	return mp, nil
}

// MountPoint returns where the filesystem is, or would be, mounted. It
// fails with *NotMountableError (ErrNotMountable) if it is not mounted
// automatically: mountpoint none or legacy, canmount=off, a volume, ...
func (c *Client) MountPoint(dataset string) (string, error) {
	d, err := c.get(dataset, false, nil)
	if err != nil {
		return "", err
	}
	return c.mountpointOf(d, false)
}

// mountOptionList builds the option list as libzfs does: the given or default
// options, then one entry per property that has a mount option.
func mountOptionList(d *Dataset, given []string) []string {
	opts := append([]string(nil), given...)
	if len(opts) == 0 {
		opts = []string{"defaults"}
	}
	has := func(names ...string) bool {
		for _, o := range opts {
			for _, n := range names {
				if o == n {
					return true
				}
			}
		}
		return false
	}
	add := func(prop, on, off string) {
		if has(on, off) {
			return
		}
		if v, ok := d.Bool(prop); ok && v {
			opts = append(opts, on)
		} else {
			opts = append(opts, off)
		}
	}
	add("atime", "atime", "noatime")
	add("devices", "devices", "nodevices")
	add("exec", "exec", "noexec")
	add("readonly", "ro", "rw")
	add("setuid", "setuid", "nosetuid")
	add("nbmand", "nbmand", "nonbmand")
	return opts
}

// readOnlyOptions makes a mount option list read-only, as zfs_mount_at does
// for a pool imported read-only: any "rw" goes, "ro" is there once.
func readOnlyOptions(list []string) []string {
	out := make([]string, 0, len(list)+1)
	for _, o := range list {
		if o != "rw" && o != "ro" {
			out = append(out, o)
		}
	}
	return append(out, "ro")
}

// Mount mounts a filesystem at its mountpoint, like zfs mount. A dataset
// that is not mountable (mountpoint none/legacy, canmount=off, ...) fails
// with a *NotMountableError; canmount=noauto is mounted, because the call is
// explicit. Mounting a mounted filesystem fails with EBUSY unless
// opts.Remount is set.
func (c *Client) Mount(dataset string, opts *MountOptions) error {
	if opts == nil {
		opts = &MountOptions{}
	}
	d, err := c.get(dataset, false, nil)
	if err != nil {
		return err
	}
	mp, err := c.mountpointOf(d, opts.Force)
	if err != nil {
		return err
	}
	return c.mountAt(d, mp, opts)
}

func (c *Client) mountAt(d *Dataset, mp string, opts *MountOptions) error {
	if c.m == nil {
		return errors.ErrUnsupported
	}
	fail := func(err error) error {
		return &MountError{Op: "mount", Dataset: d.Name, Mountpoint: mp, Err: err}
	}
	if enc, _ := d.Enum("encryption"); enc != "" && enc != "off" {
		if ks, _ := d.Enum("keystatus"); ks == "unavailable" {
			if opts.Key == nil {
				return fail(ErrKeyNotLoaded)
			}
			root, _ := d.String("encryptionroot")
			if root == "" || root == "-" {
				root = d.Name
			}
			if err := c.LoadKey(root, opts.Key); err != nil {
				return fail(err)
			}
		}
	}
	if err := c.m.mkdirAll(mp); err != nil {
		return fail(fmt.Errorf("creating the mountpoint: %w", err))
	}
	overlay := opts.Overlay
	if v, ok := d.Bool("overlay"); ok && v {
		overlay = true
	}
	if !overlay && !opts.Remount {
		empty, err := c.m.isEmptyDir(mp)
		if err != nil {
			return fail(err)
		}
		if !empty {
			return fail(ErrMountpointNotEmpty)
		}
	}
	list := mountOptionList(d, opts.Options)
	if ro, err := c.PoolReadOnly(poolOf(d.Name)); err == nil && ro {
		list = readOnlyOptions(list) // nothing can be written to the pool
	}
	if opts.Remount {
		list = append([]string{"update"}, list...)
	}
	list = append(list, "zfsutil")
	if err := c.m.nmount(d.Name, mp, list); err != nil {
		return fail(err)
	}
	return nil
}

// Unmount unmounts a filesystem. A filesystem that is not mounted is not an
// error, as with libzfs.
func (c *Client) Unmount(dataset string, opts *UnmountOptions) error {
	if opts == nil {
		opts = &UnmountOptions{}
	}
	where, ok, err := c.IsMounted(dataset)
	if err != nil {
		return err
	}
	if ok {
		if err := c.m.unmount(where, opts.Force); err != nil {
			return &MountError{Op: "unmount", Dataset: dataset, Mountpoint: where, Err: err}
		}
	}
	if opts.UnloadKey {
		d, err := c.get(dataset, false, nil)
		if err != nil {
			return err
		}
		if enc, _ := d.Enum("encryption"); enc != "" && enc != "off" {
			root, _ := d.String("encryptionroot")
			if ks, _ := d.Enum("keystatus"); (root == dataset || root == "" || root == "-") && ks == "available" {
				return c.UnloadKey(dataset)
			}
		}
	}
	return nil
}

// MountAll mounts every filesystem at or below root that is mountable and
// has canmount=on and is not yet mounted (zfs mount -a), parents before
// children. It continues after a failure and returns the failures joined.
func (c *Client) MountAll(root string, opts *MountOptions) error {
	if opts == nil {
		opts = &MountOptions{}
	}
	mounted, err := c.mountedSet()
	if err != nil {
		return err
	}
	var todo []*Dataset
	err = c.Walk(root, ListOptions{Types: Filesystem, Depth: Unlimited}, func(d *Dataset) error {
		if cm, _ := d.Enum("canmount"); cm == "on" || cm == "" {
			todo = append(todo, d)
		}
		return nil
	})
	if err != nil {
		return err
	}
	type item struct {
		d  *Dataset
		mp string
	}
	var items []item
	for _, d := range todo {
		if _, ok := mounted[d.Name]; ok {
			continue
		}
		mp, err := c.mountpointOf(d, opts.Force)
		if err != nil {
			continue // not mountable: skipped, as zfs mount -a does
		}
		items = append(items, item{d, mp})
	}
	// Shallower mountpoints first, so that a mount does not hide the
	// directory of a later one.
	sort.SliceStable(items, func(i, j int) bool {
		return strings.Count(items[i].mp, "/") < strings.Count(items[j].mp, "/")
	})
	var errs []error
	for _, it := range items {
		if err := c.mountAt(it.d, it.mp, opts); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// UnmountAll unmounts every filesystem at or below root, children first.
func (c *Client) UnmountAll(root string, opts *UnmountOptions) error {
	if opts == nil {
		opts = &UnmountOptions{}
	}
	mounted, err := c.mountedSet()
	if err != nil {
		return err
	}
	var names []string
	err = c.Walk(root, ListOptions{Types: Filesystem, Depth: Unlimited, Simple: true}, func(d *Dataset) error {
		if _, ok := mounted[d.Name]; ok {
			names = append(names, d.Name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.SliceStable(names, func(i, j int) bool {
		return strings.Count(mounted[names[i]], "/") > strings.Count(mounted[names[j]], "/")
	})
	var errs []error
	for _, n := range names {
		if err := c.Unmount(n, opts); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Client) mountedSet() (map[string]string, error) {
	ms, err := c.Mounts()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(ms))
	for _, m := range ms {
		out[m.Dataset] = m.Mountpoint
	}
	return out, nil
}

// isBusy reports whether err is EBUSY, the usual "cannot unmount" cause.
func isBusy(err error) bool { return errors.Is(err, syscall.EBUSY) }
