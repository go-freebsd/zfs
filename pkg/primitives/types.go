package primitives

import (
	"sort"
	"strings"
	"time"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
)

// Type is a kind of dataset. It is also used as a bit mask to select
// several kinds, as in ListOptions.Types.
type Type uint8

const (
	Filesystem Type = 1 << iota
	Volume
	Snapshot
	Bookmark
)

func (t Type) String() string {
	var parts []string
	for _, n := range []struct {
		t Type
		s string
	}{{Filesystem, "filesystem"}, {Volume, "volume"}, {Snapshot, "snapshot"}, {Bookmark, "bookmark"}} {
		if t&n.t != 0 {
			parts = append(parts, n.s)
		}
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, ",")
}

// Property is one property of a dataset as the kernel reports it.
type Property struct {
	// Value is a uint64 or a string; enumerated properties are numbers. The
	// "clones" property is a *nvlist.List of dataset names (see Clones).
	Value any `nvlist:"value"`
	// Source is the dataset the value is set on ("" for the default, the
	// dataset's own name for a local value, "$recvd" for a received one).
	Source string `nvlist:"source"`
}

// Dataset describes a filesystem, volume, snapshot or bookmark.
type Dataset struct {
	Name         string
	Type         Type
	GUID         uint64
	CreationTxg  uint64
	NumClones    uint64
	Origin       string // for a clone, the snapshot it was created from
	Inconsistent bool
	// Props holds every property the kernel returned (none for listings
	// with ListOptions.Simple).
	Props map[string]Property
}

// Uint64 returns a numeric property.
func (d *Dataset) Uint64(name string) (uint64, bool) {
	v, ok := d.Props[name].Value.(uint64)
	return v, ok
}

// String returns a string property.
func (d *Dataset) String(name string) (string, bool) {
	v, ok := d.Props[name].Value.(string)
	return v, ok
}

func (d *Dataset) num(name string) uint64 { v, _ := d.Uint64(name); return v }

// Space accounting and settings; zero if the property is not present.
func (d *Dataset) Used() uint64        { return d.num("used") }
func (d *Dataset) Available() uint64   { return d.num("available") }
func (d *Dataset) Referenced() uint64  { return d.num("referenced") }
func (d *Dataset) LogicalUsed() uint64 { return d.num("logicalused") }
func (d *Dataset) Quota() uint64       { return d.num("quota") }
func (d *Dataset) Reservation() uint64 { return d.num("reservation") }

// Mountpoint returns the effective mountpoint ("none", "legacy" or a path).
// The kernel stores an inherited mountpoint as the value of the ancestor it
// was set on; like zfs get, this appends the part of the name below that
// ancestor.
func (d *Dataset) Mountpoint() string {
	p := d.Props["mountpoint"]
	v, _ := p.Value.(string)
	if !strings.HasPrefix(v, "/") || p.Source == "" || p.Source == "$recvd" || p.Source == d.Name {
		return v
	}
	if suffix, ok := strings.CutPrefix(d.Name, p.Source); ok && strings.HasPrefix(suffix, "/") {
		return strings.TrimSuffix(v, "/") + suffix
	}
	return v
}

// Clones returns the names of the datasets cloned from this snapshot, sorted.
func (d *Dataset) Clones() []string {
	l, ok := d.Props["clones"].Value.(*nvlist.List)
	if !ok {
		return nil
	}
	names := l.Names()
	sort.Strings(names)
	return names
}

// Creation returns the creation time property.
func (d *Dataset) Creation() time.Time {
	if v, ok := d.Uint64("creation"); ok {
		return time.Unix(int64(v), 0)
	}
	return time.Time{}
}

// Hold is a user reference that keeps a snapshot from being destroyed.
type Hold struct {
	Tag     string
	Created time.Time
}

// Space is the result of a space accounting query, in bytes.
type Space struct {
	Used         uint64 // SnapshotRangeSpace: space freed by destroying the range
	Written      uint64 // SpaceWritten: data written between the two points
	Compressed   uint64
	Uncompressed uint64
}
