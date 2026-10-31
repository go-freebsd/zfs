package zfs

import (
	"fmt"
	"time"

	"github.com/go-freebsd/zfs/pkg/primitives"
)

// scope says which operations an Option is valid for.
type scope uint16

const (
	scCreate scope = 1 << iota
	scSet
	scClone
	scSnapshot
	scDestroyDataset
	scDestroySnapshot
	scList
	scSnapshotAll
	scSend
	scReceive
	scMount
	scUnmount
	scDestroyRange

	scProps = scCreate | scSet | scClone | scReceive // a regular property
	scUser  = scProps | scSnapshot | scSnapshotAll
)

// Option customizes an operation: properties for Create, Set, Clone and
// Snapshot, and flags for Destroy and the listing calls. An option that does
// not apply to the operation it is given to makes the call fail with
// ErrInvalidOption before anything is sent to the kernel.
type Option struct {
	name  string
	scope scope
	apply func(*config)
}

type config struct {
	props     map[string]any
	key       *Key
	parents   bool
	recursive bool
	deferred  bool
	depth     int
	types     Kind

	noMount      bool
	forceUnmount bool

	send    primitives.SendOptions
	recv    primitives.ReceiveOptions
	mount   primitives.MountOptions
	unmount primitives.UnmountOptions
}

func collect(op string, sc scope, opts []Option) (*config, error) {
	cfg := &config{props: map[string]any{}, depth: primitives.Unlimited}
	for _, o := range opts {
		if o.apply == nil {
			return nil, fmt.Errorf("%w: zero Option passed to %s", ErrInvalidOption, op)
		}
		if o.scope&sc == 0 {
			return nil, fmt.Errorf("%w: %s cannot be used with %s", ErrInvalidOption, o.name, op)
		}
		o.apply(cfg)
	}
	return cfg, nil
}

func (c *config) create() *primitives.CreateOptions {
	return &primitives.CreateOptions{Props: c.props, Key: c.key, Parents: c.parents, NoMount: c.noMount}
}

func (c *config) sendOpts() *primitives.SendOptions {
	o := c.send
	return &o
}

func (c *config) recvOpts() *primitives.ReceiveOptions {
	o := c.recv
	o.Props = c.props
	o.Parents = c.parents
	o.NoMount = c.noMount
	return &o
}

func (c *config) mountOpts() *primitives.MountOptions {
	o := c.mount
	return &o
}

func (c *config) unmountOpts() *primitives.UnmountOptions {
	o := c.unmount
	o.Force = c.forceUnmount
	return &o
}

func prop(name string, sc scope, prop string, v any) Option {
	return Option{name: name, scope: sc, apply: func(c *config) { c.props[prop] = v }}
}

// WithProp sets any property by its zfs name, with the value types
// primitives.SetProps takes. It is the way to set a user property
// ("module:name", a string) and the escape hatch for properties without an
// option of their own. It is the only property option a snapshot accepts.
func WithProp(name string, value any) Option {
	return prop("WithProp("+name+")", scUser, name, value)
}

// WithProps is WithProp for several properties.
func WithProps(props map[string]any) Option {
	return Option{name: "WithProps", scope: scUser, apply: func(c *config) {
		for k, v := range props {
			c.props[k] = v
		}
	}}
}

// Property options. Each is valid for Create, Set and Clone.

func WithCompression(v Compression) Option {
	return prop("WithCompression", scProps, "compression", string(v))
}
func WithChecksum(v Checksum) Option { return prop("WithChecksum", scProps, "checksum", string(v)) }
func WithSync(v SyncMode) Option     { return prop("WithSync", scProps, "sync", string(v)) }

// WithQuota limits the space of the dataset and its descendants; 0 removes
// the limit. WithRefQuota, WithReservation and WithRefReservation are the
// same for the other limits.
func WithQuota(n uint64) Option       { return prop("WithQuota", scProps, "quota", n) }
func WithRefQuota(n uint64) Option    { return prop("WithRefQuota", scProps, "refquota", n) }
func WithReservation(n uint64) Option { return prop("WithReservation", scProps, "reservation", n) }
func WithRefReservation(n uint64) Option {
	return prop("WithRefReservation", scProps, "refreservation", n)
}

// WithRecordSize sets the filesystem block size: a power of two from 512
// bytes to 16 MiB.
func WithRecordSize(n uint64) Option { return prop("WithRecordSize", scProps, "recordsize", n) }

// WithMountpoint sets the mountpoint: a path, "none" or "legacy".
func WithMountpoint(path string) Option {
	return prop("WithMountpoint", scProps, "mountpoint", path)
}
func WithAtime(on bool) Option     { return prop("WithAtime", scProps, "atime", on) }
func WithReadOnly(on bool) Option  { return prop("WithReadOnly", scProps, "readonly", on) }
func WithCopies(n int) Option      { return prop("WithCopies", scProps, "copies", fmt.Sprint(n)) }
func WithCanMount(v string) Option { return prop("WithCanMount", scProps, "canmount", v) } // on, off, noauto

// Create-only options.

// WithVolBlockSize sets the block size of a volume.
func WithVolBlockSize(n uint64) Option {
	return prop("WithVolBlockSize", scCreate, "volblocksize", n)
}

// WithCaseSensitivity is "sensitive", "insensitive" or "mixed".
func WithCaseSensitivity(v string) Option {
	return prop("WithCaseSensitivity", scCreate, "casesensitivity", v)
}

// WithUTF8Only rejects file names that are not valid UTF-8.
func WithUTF8Only(on bool) Option { return prop("WithUTF8Only", scCreate, "utf8only", on) }

// Parents creates the missing parent filesystems too, as zfs create -p.
func Parents() Option {
	return Option{name: "Parents", scope: scCreate | scClone | scReceive, apply: func(c *config) { c.parents = true }}
}

// WithEncryption creates an encrypted dataset with the algorithm and key.
// The key format (raw, hex or passphrase) comes from the key. A dataset
// below an encrypted parent needs no option; it inherits the parent's.
func WithEncryption(alg Algorithm, key *Key) Option {
	return Option{name: "WithEncryption", scope: scCreate, apply: func(c *config) {
		c.props["encryption"] = string(alg)
		c.key = key
	}}
}

// NoMount leaves a filesystem unmounted. Create, Clone and Receive mount
// what they made (when its mountpoint is a path and canmount is on), as the
// zfs commands do; this is zfs receive -u and zfs create -u.
func NoMount() Option {
	return Option{name: "NoMount", scope: scCreate | scClone | scReceive, apply: func(c *config) { c.noMount = true }}
}

// Destroy options.

// ForceUnmount unmounts busy filesystems first (zfs destroy -f, unmount -f).
// Without it a busy mount makes the call fail with ErrBusy.
func ForceUnmount() Option {
	return Option{name: "ForceUnmount", scope: scDestroyDataset | scUnmount, apply: func(c *config) { c.forceUnmount = true }}
}

// Recursive destroys the descendants and snapshots of a dataset too.
func Recursive() Option {
	return Option{name: "Recursive", scope: scDestroyDataset | scDestroyRange, apply: func(c *config) { c.recursive = true }}
}

// Deferred marks a snapshot that is held or cloned for destruction once
// the last hold or clone is gone.
func Deferred() Option {
	return Option{name: "Deferred", scope: scDestroySnapshot | scDestroyRange, apply: func(c *config) { c.deferred = true }}
}

// Listing options.

// Depth limits how many levels below the dataset are visited; 1 is the
// children only.
func Depth(n int) Option {
	return Option{name: "Depth", scope: scList, apply: func(c *config) { c.depth = n }}
}

// OnlyKinds restricts a listing to filesystems (KindFilesystem), volumes
// (KindVolume) or both.
func OnlyKinds(k Kind) Option {
	return Option{name: "OnlyKinds", scope: scList, apply: func(c *config) { c.types = k }}
}

// Send options.

// SendFrom makes the stream incremental: only what changed since from, an
// earlier snapshot or a bookmark of the same dataset (zfs send -i).
func SendFrom(from Pointer) Option {
	return Option{name: "SendFrom", scope: scSend, apply: func(c *config) { c.send.From = from.Name() }}
}

// SendIntermediates sends every snapshot between from and the one sent, in
// one replication stream (like zfs send -I). from must be a snapshot. Receive
// takes the result, and so does zfs receive.
func SendIntermediates(from *Snapshot) Option {
	return Option{name: "SendIntermediates", scope: scSend, apply: func(c *config) {
		c.send.From, c.send.Intermediates = from.Name(), true
	}}
}

// Replicate sends the dataset of the snapshot with everything below it: the
// descendants, their snapshots, clones and properties (zfs send -R). The
// result is one replication stream, which Dataset.Receive takes apart.
// Combined with SendFrom it is incremental.
func Replicate() Option {
	return Option{name: "Replicate", scope: scSend, apply: func(c *config) { c.send.Replicate = true }}
}

// SendProps sends the properties of the dataset and the snapshot in front of
// the stream (zfs send -p).
func SendProps() Option {
	return Option{name: "SendProps", scope: scSend, apply: func(c *config) { c.send.Props = true }}
}

// SendHolds sends the holds on the snapshots too (zfs send -h); a receive
// puts them back unless it uses SkipHolds.
func SendHolds() Option {
	return Option{name: "SendHolds", scope: scSend, apply: func(c *config) { c.send.Holds = true }}
}

// SendBackup sends the properties as they were received instead of the ones
// in effect, and only for datasets that were received (zfs send -b). Use it
// to back up what a restore should bring back, without the local settings of
// the sender; it writes the header like SendProps.
func SendBackup() Option {
	return Option{name: "SendBackup", scope: scSend, apply: func(c *config) { c.send.Backup = true }}
}

// SendDedup is accepted and ignored, as zfs send -D is: deduplicated streams
// are no longer made. It exists so that command lines can be ported as they
// are; the stream is the same with or without it.
//
// Deprecated: it has no effect.
func SendDedup() Option {
	return Option{name: "SendDedup", scope: scSend, apply: func(c *config) {}}
}

// SkipMissing skips descendants that lack the snapshot, with a warning,
// instead of failing (zfs send --skip-missing). The warnings come back as an
// error matching ErrSendIncomplete after the stream is written.
func SkipMissing() Option {
	return Option{name: "SkipMissing", scope: scSend, apply: func(c *config) { c.send.SkipMissing = true }}
}

// SkipHolds does not put back the holds a replication stream carries
// (zfs receive -k).
func SkipHolds() Option {
	return Option{name: "SkipHolds", scope: scReceive, apply: func(c *config) { c.recv.SkipHolds = true }}
}

// LargeBlocks allows blocks larger than 128 KiB in the stream (zfs send -L).
func LargeBlocks() Option {
	return Option{name: "LargeBlocks", scope: scSend, apply: func(c *config) { c.send.LargeBlocks = true }}
}

// EmbeddedData allows small blocks to be embedded in the stream records
// (zfs send -e).
func EmbeddedData() Option {
	return Option{name: "EmbeddedData", scope: scSend, apply: func(c *config) { c.send.EmbedData = true }}
}

// Compressed sends blocks as they are compressed on disk (zfs send -c).
func Compressed() Option {
	return Option{name: "Compressed", scope: scSend, apply: func(c *config) { c.send.Compress = true }}
}

// Raw sends the dataset as stored: an encrypted dataset stays encrypted and
// can be received without its key (zfs send -w).
func Raw() Option {
	return Option{name: "Raw", scope: scSend, apply: func(c *config) { c.send.Raw = true }}
}

// Saved sends the saved state of a partially received dataset
// (zfs send --saved); see Dataset.SendSaved.
func Saved() Option {
	return Option{name: "Saved", scope: scSend, apply: func(c *config) { c.send.Saved = true }}
}

// RedactedWith sends a redacted stream: the blocks that differ in the
// snapshots the bookmark was made with (Snapshot.Redact) are left out
// (zfs send --redact).
func RedactedWith(b *Bookmark) Option {
	return Option{name: "RedactedWith", scope: scSend, apply: func(c *config) { c.send.RedactBookmark = b.Name() }}
}

// OnProgress calls fn with the bytes written so far, about every interval
// (100 ms if zero) while a send runs. fn runs on its own goroutine.
func OnProgress(interval time.Duration, fn func(SendProgress)) Option {
	return Option{name: "OnProgress", scope: scSend, apply: func(c *config) {
		c.send.Progress, c.send.ProgressInterval = fn, interval
	}}
}

// Receive options. The property options (WithCompression, WithMountpoint,
// WithProp, ...) set properties locally on the received dataset, as zfs
// receive -o does; Parents creates missing parent filesystems.

// Force rolls the target back to its latest snapshot first, destroying
// newer snapshots and changes, so that an incremental stream applies
// (zfs receive -F).
func Force() Option {
	return Option{name: "Force", scope: scReceive, apply: func(c *config) { c.recv.Force = true }}
}

// Resumable keeps what was received if the transfer is interrupted, so that
// it can continue (zfs receive -s). See Dataset.ResumeToken.
func Resumable() Option {
	return Option{name: "Resumable", scope: scReceive, apply: func(c *config) { c.recv.Resumable = true }}
}

// Heal repairs corrupted blocks of the snapshot from a stream of it
// (zfs receive -c).
func Heal() Option {
	return Option{name: "Heal", scope: scReceive, apply: func(c *config) { c.recv.Heal = true }}
}

// WithOrigin makes a new dataset a clone of the snapshot
// (zfs receive -o origin=).
func WithOrigin(s *Snapshot) Option {
	return Option{name: "WithOrigin", scope: scReceive, apply: func(c *config) { c.recv.Origin = s.Name() }}
}

// DiscardPool puts the path of the dataset in the stream, without the pool
// name, below the target (zfs receive -d).
func DiscardPool() Option {
	return Option{name: "DiscardPool", scope: scReceive, apply: func(c *config) { c.recv.Layout = primitives.LayoutDiscardPool }}
}

// Exclude names properties that the stream must not set (zfs receive -x):
// they are inherited on the target instead. A plain stream carries no
// properties, so this changes something only for a target that already
// holds received values; see primitives.ReceiveOptions.Exclude.
func Exclude(props ...string) Option {
	return Option{name: "Exclude", scope: scReceive, apply: func(c *config) { c.recv.Exclude = append(c.recv.Exclude, props...) }}
}

// DryRun checks the stream and where it would go and reads it to its end,
// but receives and mounts nothing (zfs receive -n). The Received values have
// DryRun set. See primitives.ReceiveOptions.DryRun for what is checked.
func DryRun() Option {
	return Option{name: "DryRun", scope: scReceive, apply: func(c *config) { c.recv.DryRun = true }}
}

// LastElement keeps only the last element of the dataset path in the stream
// (zfs receive -e).
func LastElement() Option {
	return Option{name: "LastElement", scope: scReceive, apply: func(c *config) { c.recv.Layout = primitives.LayoutLastElement }}
}

// Mount options.

// Overlay allows mounting over a directory that has content (zfs mount -O).
func Overlay() Option {
	return Option{name: "Overlay", scope: scMount, apply: func(c *config) { c.mount.Overlay = true }}
}

// Remount updates an existing mount (zfs mount -o remount).
func Remount() Option {
	return Option{name: "Remount", scope: scMount, apply: func(c *config) { c.mount.Remount = true }}
}

// WithMountOptions replaces the default mount options ("defaults"); the
// options the properties imply are still added.
func WithMountOptions(opts ...string) Option {
	return Option{name: "WithMountOptions", scope: scMount, apply: func(c *config) { c.mount.Options = append(c.mount.Options, opts...) }}
}

// WithMountKey loads the key first if the dataset is encrypted and its key
// is not loaded (zfs mount -l).
func WithMountKey(key *Key) Option {
	return Option{name: "WithMountKey", scope: scMount, apply: func(c *config) { c.mount.Key = key }}
}

// MountRedacted also mounts a redacted dataset (zfs mount -f).
func MountRedacted() Option {
	return Option{name: "MountRedacted", scope: scMount, apply: func(c *config) { c.mount.Force = true }}
}

// AndUnloadKey also unloads the key of an encryption root after unmounting
// it (zfs unmount -u).
func AndUnloadKey() Option {
	return Option{name: "AndUnloadKey", scope: scUnmount, apply: func(c *config) { c.unmount.UnloadKey = true }}
}
