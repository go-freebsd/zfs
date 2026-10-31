package zfs

import (
	"context"
	"iter"
	"strings"

	"github.com/go-freebsd/zfs/pkg/primitives"
)

// Snapshot is a handle to a snapshot, "pool/path@name".
type Snapshot struct {
	c    *Client
	name string
}

// Name is the full name.
func (s *Snapshot) Name() string { return s.name }

// String returns the name.
func (s *Snapshot) String() string { return s.name }

// ShortName is the part after the @.
func (s *Snapshot) ShortName() string {
	_, short, _ := strings.Cut(s.name, "@")
	return short
}

// Dataset returns the handle of the dataset the snapshot is of.
func (s *Snapshot) Dataset() *Dataset {
	ds, _, _ := strings.Cut(s.name, "@")
	return s.c.Dataset(ds)
}

// Exists reports whether the snapshot exists.
func (s *Snapshot) Exists() (bool, error) {
	ok, err := s.c.b.Exists(s.name)
	return ok, wrap("exists", s.name, err)
}

// Info reads the snapshot's properties.
func (s *Snapshot) Info() (*Info, error) {
	raw, err := s.c.b.Get(s.name)
	if err != nil {
		return nil, wrap("get", s.name, err)
	}
	return newInfo(s.c, raw), nil
}

// Set changes user properties of the snapshot (WithProp).
func (s *Snapshot) Set(opts ...Option) error {
	cfg, err := collect("set", scSnapshot, opts)
	if err != nil {
		return wrap("set", s.name, err)
	}
	return wrap("set", s.name, s.c.b.SetProps(s.name, cfg.props))
}

// Destroy destroys the snapshot. With Deferred(), a snapshot that is held or
// cloned is marked and goes away with its last hold or clone.
func (s *Snapshot) Destroy(opts ...Option) error {
	cfg, err := collect("destroy", scDestroySnapshot, opts)
	if err != nil {
		return wrap("destroy", s.name, err)
	}
	return wrap("destroy", s.name, s.c.b.Destroy(s.name, &primitives.DestroyOptions{Defer: cfg.deferred}))
}

// Clone creates a writable filesystem or volume from the snapshot. name is
// the full name of the clone, in the same pool.
func (s *Snapshot) Clone(name string, opts ...Option) (*Dataset, error) {
	cfg, err := collect("clone", scClone, opts)
	if err != nil {
		return nil, wrap("clone", name, err)
	}
	if err := s.c.b.Clone(name, s.name, cfg.create()); err != nil {
		return nil, wrap("clone", name, err)
	}
	return s.c.Dataset(name), nil
}

// Rollback returns the dataset to this snapshot. It fails with ErrExists if
// newer snapshots exist; destroy them first.
func (s *Snapshot) Rollback() error {
	_, err := s.c.b.Rollback(s.Dataset().name, s.name)
	return wrap("rollback", s.name, err)
}

// Hold puts a hold named tag on the snapshot, which then cannot be
// destroyed until the hold is released.
func (s *Snapshot) Hold(tag string) error { return wrap("hold", s.name, s.c.b.Hold(tag, s.name)) }

// Release removes the hold named tag.
func (s *Snapshot) Release(tag string) error {
	return wrap("release", s.name, s.c.b.Release(tag, s.name))
}

// Holds lists the holds, ordered by tag.
func (s *Snapshot) Holds() ([]Hold, error) {
	h, err := s.c.b.Holds(s.name)
	return h, wrap("holds", s.name, err)
}

// Bookmark creates the bookmark dataset#name from the snapshot. A bookmark
// keeps the point in time for incremental sends after the snapshot is gone.
func (s *Snapshot) Bookmark(name string) (*Bookmark, error) {
	bm := s.c.Bookmark(s.Dataset().name + "#" + name)
	if err := s.c.b.CreateBookmark(bm.name, s.name); err != nil {
		return nil, wrap("bookmark", bm.name, err)
	}
	return bm, nil
}

// Pointer is a snapshot or a bookmark, a point in time to measure from.
type Pointer interface{ Name() string }

// WrittenSince returns the data written between from (an earlier snapshot
// or bookmark of the same dataset) and this snapshot.
func (s *Snapshot) WrittenSince(from Pointer) (Space, error) {
	sp, err := s.c.b.SpaceWritten(s.name, from.Name())
	return sp, wrap("written", s.name, err)
}

// Bookmark is a handle to a bookmark, "pool/path#name".
type Bookmark struct {
	c    *Client
	name string
}

// Name is the full name.
func (b *Bookmark) Name() string { return b.name }

// String returns the name.
func (b *Bookmark) String() string { return b.name }

// Dataset returns the handle of the dataset the bookmark belongs to.
func (b *Bookmark) Dataset() *Dataset {
	ds, _, _ := strings.Cut(b.name, "#")
	return b.c.Dataset(ds)
}

// Info reads the bookmark's guid, createtxg and creation time.
func (b *Bookmark) Info() (*Info, error) {
	raw, err := b.c.b.Get(b.name)
	if err != nil {
		return nil, wrap("get", b.name, err)
	}
	return newInfo(b.c, raw), nil
}

// Destroy destroys the bookmark.
func (b *Bookmark) Destroy() error { return wrap("destroy", b.name, b.c.b.Destroy(b.name, nil)) }

// Diff lists what changed between s and the later snapshot other of the same
// dataset, or of a clone of it (zfs diff). Entries come in the order of the
// objects, not of the names; paths start at the root of the dataset and are
// not escaped. Breaking out of the loop or cancelling ctx ends the diff.
func (s *Snapshot) Diff(ctx context.Context, other *Snapshot) iter.Seq2[DiffEntry, error] {
	return s.diff(ctx, other.name)
}

// DiffNow lists what changed between s and the live filesystem (zfs diff
// with one snapshot). A temporary snapshot is made for it and removed again.
func (s *Snapshot) DiffNow(ctx context.Context) iter.Seq2[DiffEntry, error] {
	return s.diff(ctx, "")
}

func (s *Snapshot) diff(ctx context.Context, to string) iter.Seq2[DiffEntry, error] {
	return func(yield func(DiffEntry, error) bool) {
		for e, err := range s.c.b.Diff(ctx, s.name, to) {
			if err != nil {
				err = wrap("diff", s.name, err)
			}
			if !yield(e, err) {
				return
			}
		}
	}
}
