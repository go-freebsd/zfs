package zfs

import (
	"fmt"

	"github.com/go-freebsd/zfs/pkg/primitives"
)

// scope says which operations an Option is valid for.
type scope uint8

const (
	scCreate scope = 1 << iota
	scSet
	scClone
	scSnapshot
	scDestroyDataset
	scDestroySnapshot
	scList
	scSnapshotAll

	scProps = scCreate | scSet | scClone // a regular property
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
	return &primitives.CreateOptions{Props: c.props, Key: c.key, Parents: c.parents}
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
	return Option{name: "Parents", scope: scCreate | scClone, apply: func(c *config) { c.parents = true }}
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

// Destroy options.

// Recursive destroys the descendants and snapshots of a dataset too.
func Recursive() Option {
	return Option{name: "Recursive", scope: scDestroyDataset, apply: func(c *config) { c.recursive = true }}
}

// Deferred marks a snapshot that is held or cloned for destruction once
// the last hold or clone is gone.
func Deferred() Option {
	return Option{name: "Deferred", scope: scDestroySnapshot, apply: func(c *config) { c.deferred = true }}
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
