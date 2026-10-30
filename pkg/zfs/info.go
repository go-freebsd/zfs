package zfs

import (
	"time"

	"github.com/go-freebsd/zfs/pkg/primitives"
)

// Info is a snapshot of the state of a dataset, snapshot or bookmark at the
// time it was read. Fields that do not apply to the kind (a snapshot has no
// compression property of its own) are zero. Sizes are in bytes; for the
// limits (Quota, RefQuota, Reservation, RefReservation) 0 means none.
type Info struct {
	Name        string
	Kind        Kind
	GUID        uint64
	CreationTxg uint64
	Created     time.Time

	Used        uint64
	Available   uint64
	Referenced  uint64
	LogicalUsed uint64
	Written     uint64

	Quota          uint64
	RefQuota       uint64
	Reservation    uint64
	RefReservation uint64
	RecordSize     uint64
	VolSize        uint64
	VolBlockSize   uint64
	Copies         uint64

	Compression   Compression
	CompressRatio float64
	Checksum      Checksum
	Sync          SyncMode
	Mountpoint    string
	ReadOnly      bool
	Atime         bool

	// Encryption is EncryptionOff for an unencrypted dataset.
	Encryption     Algorithm
	KeyStatus      KeyStatus
	EncryptionRoot string

	// Origin is the snapshot a clone was created from; Clones are the
	// datasets cloned from a snapshot.
	Origin string
	Clones []string

	c   *Client
	raw *primitives.Dataset
}

func newInfo(c *Client, d *primitives.Dataset) *Info {
	in := &Info{
		c: c, raw: d, Name: d.Name, Kind: d.Type, GUID: d.GUID, CreationTxg: d.CreationTxg,
		Origin: d.Origin, Clones: d.Clones(),
	}
	if t := d.Creation(); !t.IsZero() {
		in.Created = t
	}
	num := func(p string) uint64 { v, _ := d.Uint64(p); return v }
	in.Used, in.Available, in.Referenced = d.Used(), d.Available(), d.Referenced()
	in.LogicalUsed, in.Written = d.LogicalUsed(), num("written")
	if d.Type&(primitives.Filesystem|primitives.Volume) == 0 {
		return in
	}
	in.Quota, in.RefQuota, in.Reservation, in.RefReservation = d.Quota(), num("refquota"), d.Reservation(), num("refreservation")
	in.RecordSize, in.VolSize, in.VolBlockSize, in.Copies = num("recordsize"), num("volsize"), num("volblocksize"), num("copies")
	enum := func(p string) string { v, _ := d.Enum(p); return v }
	in.Compression, in.Checksum, in.Sync = Compression(enum("compression")), Checksum(enum("checksum")), SyncMode(enum("sync"))
	in.CompressRatio, _ = d.Ratio("compressratio")
	in.ReadOnly, _ = d.Bool("readonly")
	in.Atime, _ = d.Bool("atime")
	if d.Type == primitives.Filesystem {
		in.Mountpoint = d.Mountpoint()
	}
	in.Encryption = Algorithm(enum("encryption"))
	in.KeyStatus = KeyStatus(enum("keystatus"))
	if in.Encryption != EncryptionOff {
		in.EncryptionRoot, _ = d.String("encryptionroot")
	}
	return in
}

// Encrypted reports whether the dataset is encrypted.
func (i *Info) Encrypted() bool { return i.Encryption != "" && i.Encryption != EncryptionOff }

// Prop returns a property as primitives.Dataset.Value does: a bool, string,
// uint64, time.Time, float64 or []string, depending on the property.
func (i *Info) Prop(name string) (any, bool) { return i.raw.Value(name) }

// Source says where a property's value comes from.
func (i *Info) Source(name string) (primitives.Source, bool) { return i.raw.Source(name) }

// Raw returns the low-level view, with every property as the kernel sent it.
func (i *Info) Raw() *primitives.Dataset { return i.raw }

// Dataset returns the handle of a filesystem or volume.
func (i *Info) Dataset() *Dataset { return i.c.Dataset(i.Name) }

// Snapshot returns the handle of a snapshot.
func (i *Info) Snapshot() *Snapshot { return i.c.Snapshot(i.Name) }
