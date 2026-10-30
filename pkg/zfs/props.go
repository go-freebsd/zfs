package zfs

import (
	"fmt"
	"strings"
	"time"
)

// PropKind says how a property's raw value is to be understood.
type PropKind uint8

const (
	KindString PropKind = iota + 1 // a string
	KindNumber                     // a count, id or other plain number
	KindSize                       // a number of bytes
	KindBool                       // an on/off (or yes/no) switch
	KindEnum                       // one of several named values stored as numbers
	KindTime                       // seconds since the epoch
	KindRatio                      // a ratio stored in hundredths (compressratio)
	KindList                       // a list of dataset names (clones)
	KindUser                       // a user property ("module:name"), always a string
	KindOpaque                     // returned as the kernel gives it (arrays, internal values)
)

func (k PropKind) String() string {
	switch k {
	case KindString:
		return "string"
	case KindNumber:
		return "number"
	case KindSize:
		return "size"
	case KindBool:
		return "bool"
	case KindEnum:
		return "enum"
	case KindTime:
		return "time"
	case KindRatio:
		return "ratio"
	case KindList:
		return "list"
	case KindUser:
		return "user"
	case KindOpaque:
		return "opaque"
	}
	return "unknown"
}

// PropValue is one named value of a KindBool or KindEnum property. When
// several names share a number, the first is the one reported.
type PropValue struct {
	Name string
	Num  uint64
}

// NoneMeans tells what the word "none" stands for when setting a number.
type NoneMeans uint8

const (
	NoneInvalid NoneMeans = iota // "none" is not accepted
	NoneIsZero                   // quota, reservation: 0 means none
	NoneIsMax                    // limits: the maximum value means none
)

// PropInfo describes a property.
type PropInfo struct {
	Name       string
	Kind       PropKind
	Values     []PropValue // for KindBool and KindEnum; do not modify
	ReadOnly   bool        // computed by ZFS, cannot be set
	CreateOnly bool        // can only be given when the dataset is created
	Inherits   bool        // takes its value from the parent unless set
	Types      Type        // the kinds of dataset it applies to
	None       NoneMeans

	// HasDefault says the property has a built-in default. The kernel does
	// not send properties that still have their default, so the typed
	// getters fall back to it: Default for numbers, sizes, switches and
	// enumerations, DefaultStr for strings.
	HasDefault bool
	Default    uint64
	DefaultStr string
}

// withDefault sets the default of a switch or enumeration by value name.
func (p PropInfo) withDefault(name string) PropInfo {
	n, ok := p.valueNum(name)
	if !ok {
		panic("zfs: property table: " + p.Name + " has no value " + name)
	}
	p.HasDefault, p.Default = true, n
	return p
}

// withDefaultNum sets the numeric default of a size or number.
func (p PropInfo) withDefaultNum(n uint64) PropInfo {
	p.HasDefault, p.Default = true, n
	return p
}

// withDefaultStr sets the default of a string property.
func (p PropInfo) withDefaultStr(s string) PropInfo {
	p.HasDefault, p.DefaultStr = true, s
	return p
}

// defaultValue returns the built-in default as the kernel would store it.
func (p PropInfo) defaultValue() (any, bool) {
	if !p.HasDefault {
		return nil, false
	}
	if p.Kind == KindString {
		return p.DefaultStr, true
	}
	return p.Default, true
}

func (p PropInfo) valueName(n uint64) (string, bool) {
	for _, v := range p.Values {
		if v.Num == n {
			return v.Name, true
		}
	}
	return "", false
}

func (p PropInfo) valueNum(name string) (uint64, bool) {
	for _, v := range p.Values {
		if v.Name == name {
			return v.Num, true
		}
	}
	return 0, false
}

func (p PropInfo) valueNames() []string {
	names := make([]string, len(p.Values))
	for i, v := range p.Values {
		names[i] = v.Name
	}
	return names
}

var propIndex = func() map[string]*PropInfo {
	m := make(map[string]*PropInfo, len(propTable))
	for i := range propTable {
		m[propTable[i].Name] = &propTable[i]
	}
	return m
}()

// familyPrefixes are properties that exist per user, group, project or
// snapshot: written@snap, userused@alice, ...
var familyPrefixes = []struct {
	prefix string
	ro     bool
}{
	{"written@", true}, {"written#", true},
	{"userused@", true}, {"groupused@", true}, {"projectused@", true},
	{"userobjused@", true}, {"groupobjused@", true}, {"projectobjused@", true},
	{"userquota@", false}, {"groupquota@", false}, {"projectquota@", false},
	{"userobjquota@", false}, {"groupobjquota@", false}, {"projectobjquota@", false},
}

// LookupProp returns what is known about a property: the built-in table,
// the per-user and per-snapshot families, and user properties.
func LookupProp(name string) (PropInfo, bool) {
	if p, ok := propIndex[name]; ok {
		return *p, true
	}
	for _, f := range familyPrefixes {
		if len(name) > len(f.prefix) && strings.HasPrefix(name, f.prefix) {
			return PropInfo{Name: name, Kind: KindSize, ReadOnly: f.ro, Types: tAll}, true
		}
	}
	if IsUserProp(name) {
		return PropInfo{Name: name, Kind: KindUser, Inherits: true, Types: tAll}, true
	}
	return PropInfo{}, false
}

// IsUserProp reports whether name is a valid user property name: it has a
// colon and only letters, digits and "-_.:" (as zfs_prop_user requires).
func IsUserProp(name string) bool {
	if !strings.Contains(name, ":") || len(name) >= 256 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

// Info returns the metadata of a property of this dataset.
func (d *Dataset) Info(name string) (PropInfo, bool) { return LookupProp(name) }

// stored returns the property as the kernel returned it, or else its
// built-in default (the kernel leaves out properties that have their default).
func (d *Dataset) stored(name string) (any, bool) {
	if p, ok := d.Props[name]; ok {
		return p.Value, true
	}
	if info, ok := LookupProp(name); ok {
		return info.defaultValue()
	}
	return nil, false
}

func (d *Dataset) storedNum(name string) (uint64, bool) {
	v, ok := d.stored(name)
	n, isNum := v.(uint64)
	return n, ok && isNum
}

// Bool returns an on/off or yes/no property as a bool.
func (d *Dataset) Bool(name string) (bool, bool) {
	info, ok := LookupProp(name)
	n, isNum := d.storedNum(name)
	if !ok || info.Kind != KindBool || !isNum {
		return false, false
	}
	return n != 0, true
}

// Enum returns an enumerated property as its name ("lz4", "standard", ...).
// For on/off properties it returns "on" or "off". ok is false when the
// property is not an enumeration, is absent without a default, or has a
// value the library does not know (use Uint64 for the raw number then).
//
// The "type" property is answered from the dataset itself ("filesystem",
// "volume", "snapshot" or "bookmark"): the kernel stores the type of the
// underlying object set, which is the same for a snapshot and its filesystem.
func (d *Dataset) Enum(name string) (string, bool) {
	if name == "type" {
		if d.Type == 0 {
			return "", false
		}
		return d.Type.String(), true
	}
	info, ok := LookupProp(name)
	n, isNum := d.storedNum(name)
	if !ok || !isNum || (info.Kind != KindEnum && info.Kind != KindBool) {
		return "", false
	}
	return info.valueName(n)
}

// Time returns a timestamp property such as creation.
func (d *Dataset) Time(name string) (time.Time, bool) {
	info, ok := LookupProp(name)
	n, isNum := d.storedNum(name)
	if !ok || info.Kind != KindTime || !isNum {
		return time.Time{}, false
	}
	return time.Unix(int64(n), 0), true
}

// Ratio returns a ratio property such as compressratio (1.5 for "1.50x").
func (d *Dataset) Ratio(name string) (float64, bool) {
	info, ok := LookupProp(name)
	n, isNum := d.storedNum(name)
	if !ok || info.Kind != KindRatio || !isNum {
		return 0, false
	}
	return float64(n) / 100, true
}

// Value returns a property in the Go type that suits it:
//
//	KindBool             bool
//	KindEnum             string (the name; the raw uint64 if the number is unknown)
//	KindSize, KindNumber uint64
//	KindTime             time.Time
//	KindRatio            float64
//	KindString, KindUser string
//	KindList             []string
//
// A property that still has its default is not sent by the kernel; Value
// then returns the built-in default (see Source). Properties the library
// does not know are returned as the kernel gave them.
func (d *Dataset) Value(name string) (any, bool) {
	raw, ok := d.stored(name)
	if !ok {
		return nil, false
	}
	info, known := LookupProp(name)
	if !known {
		return raw, true
	}
	switch info.Kind {
	case KindBool:
		if v, ok := d.Bool(name); ok {
			return v, true
		}
	case KindEnum:
		if v, ok := d.Enum(name); ok {
			return v, true
		}
	case KindTime:
		if v, ok := d.Time(name); ok {
			return v, true
		}
	case KindRatio:
		if v, ok := d.Ratio(name); ok {
			return v, true
		}
	case KindList:
		return d.Clones(), true
	}
	return raw, true
}

// SourceKind says where a property's value comes from.
type SourceKind uint8

const (
	SourceDefault   SourceKind = iota // the built-in default (also read-only properties)
	SourceLocal                       // set on this dataset
	SourceInherited                   // taken from an ancestor
	SourceReceived                    // set by zfs receive
)

// Source is where a property's value comes from.
type Source struct {
	Kind SourceKind
	From string // for SourceInherited, the ancestor
}

func (s Source) String() string {
	switch s.Kind {
	case SourceLocal:
		return "local"
	case SourceInherited:
		return "inherited from " + s.From
	case SourceReceived:
		return "received"
	}
	return "default"
}

// Source returns where the property's value comes from.
func (d *Dataset) Source(name string) (Source, bool) {
	p, ok := d.Props[name]
	if !ok {
		// Not sent by the kernel: it has its default value, if it has one.
		if _, has := d.stored(name); has {
			return Source{Kind: SourceDefault}, true
		}
		return Source{}, false
	}
	switch {
	case p.Source == "":
		return Source{Kind: SourceDefault}, true
	case p.Source == "$recvd":
		return Source{Kind: SourceReceived}, true
	case p.Source == d.Name:
		return Source{Kind: SourceLocal}, true
	}
	return Source{Kind: SourceInherited, From: p.Source}, true
}

func (i PropInfo) String() string {
	return fmt.Sprintf("%s (%s)", i.Name, i.Kind)
}
