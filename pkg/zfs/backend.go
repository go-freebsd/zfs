package zfs

import (
	"context"
	"io"
	"iter"

	"github.com/go-freebsd/zfs/pkg/primitives"
)

// backend is the part of *primitives.Client this package uses. It exists so
// the unit tests can record what a call turns into.
type backend interface {
	Close() error
	Pools() ([]string, error)
	Get(name string, props ...string) (*primitives.Dataset, error)
	Exists(name string) (bool, error)
	Walk(root string, opts primitives.ListOptions, fn func(*primitives.Dataset) error) error
	Bookmarks(fs string, props ...string) ([]*primitives.Dataset, error)
	Holds(snapshot string) ([]primitives.Hold, error)
	SpaceWritten(newer, older string) (primitives.Space, error)

	Create(name string, opts *primitives.CreateOptions) error
	CreateVolume(name string, size uint64, opts *primitives.CreateOptions) error
	Clone(name, origin string, opts *primitives.CreateOptions) error
	Snapshot(names []string, props map[string]any) error
	Destroy(name string, opts *primitives.DestroyOptions) error
	Rename(oldName, newName string, recursive bool) error
	Rollback(dataset, snapshot string) (string, error)
	Promote(clone string) error
	Hold(tag string, snapshots ...string) error
	Release(tag string, snapshots ...string) error
	CreateBookmark(bookmark, source string) error

	SetProps(dataset string, props map[string]any) error
	InheritProp(dataset, prop string) error

	Send(ctx context.Context, snapshot string, w io.Writer, o *primitives.SendOptions) error
	SendSize(snapshot string, o *primitives.SendOptions) (uint64, error)
	Diff(ctx context.Context, from, to string) iter.Seq2[primitives.DiffEntry, error]
	SendSizes(snapshot string, o *primitives.SendOptions) ([]primitives.StreamSize, error)
	ResumeSend(ctx context.Context, w io.Writer, token string, o *primitives.SendOptions) error
	Redact(snapshot, bookmark string, redactionSnapshots []string) error
	Receive(ctx context.Context, r io.Reader, target string, o *primitives.ReceiveOptions) ([]primitives.Received, error)
	ResumeToken(dataset string) (string, bool, error)
	AbortResume(dataset string) error

	Mounts() ([]primitives.Mount, error)
	IsMounted(dataset string) (string, bool, error)
	MountPoint(dataset string) (string, error)
	Mount(dataset string, o *primitives.MountOptions) error
	Unmount(dataset string, o *primitives.UnmountOptions) error
	MountAll(root string, o *primitives.MountOptions) error
	UnmountAll(root string, o *primitives.UnmountOptions) error

	LoadKey(dataset string, key *primitives.Key) error
	CheckKey(dataset string, key *primitives.Key) error
	UnloadKey(dataset string) error
	ChangeKey(dataset string, key *primitives.Key) error
	InheritKey(dataset string) error
}

var _ backend = (*primitives.Client)(nil)
