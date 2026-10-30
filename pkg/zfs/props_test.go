package zfs

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

func TestPropTableIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range propTable {
		if p.Name == "" || seen[p.Name] {
			t.Errorf("empty or duplicate property name %q", p.Name)
		}
		seen[p.Name] = true
		if p.Types == 0 {
			t.Errorf("%s: no dataset types", p.Name)
		}
		if p.ReadOnly && (p.CreateOnly || p.Inherits) {
			t.Errorf("%s: a read-only property is neither create-only nor inheritable", p.Name)
		}
		switch p.Kind {
		case KindBool:
			if len(p.Values) != 2 || p.Values[0].Num != 0 || p.Values[1].Num != 1 {
				t.Errorf("%s: a bool needs exactly the values 0 and 1: %v", p.Name, p.Values)
			}
		case KindEnum:
			if len(p.Values) < 2 {
				t.Errorf("%s: an enumeration needs values", p.Name)
			}
			names := map[string]bool{}
			for _, v := range p.Values {
				if v.Name == "" || names[v.Name] {
					t.Errorf("%s: empty or duplicate value name %q", p.Name, v.Name)
				}
				names[v.Name] = true
			}
		case KindString, KindNumber, KindSize, KindTime, KindRatio, KindList, KindOpaque:
			if len(p.Values) != 0 {
				t.Errorf("%s: only bools and enumerations have values", p.Name)
			}
		default:
			t.Errorf("%s: invalid kind %d", p.Name, p.Kind)
		}
		if p.HasDefault {
			switch p.Kind {
			case KindBool, KindEnum:
				if _, ok := p.valueName(p.Default); !ok {
					t.Errorf("%s: the default %d is not one of its values", p.Name, p.Default)
				}
			case KindString:
				if p.Default != 0 {
					t.Errorf("%s: a string property has a string default", p.Name)
				}
			case KindSize, KindNumber:
				if p.DefaultStr != "" {
					t.Errorf("%s: a number has a numeric default", p.Name)
				}
			default:
				t.Errorf("%s: a %v has no default", p.Name, p.Kind)
			}
		}
		if p.ReadOnly && p.HasDefault && p.Kind != KindBool {
			t.Errorf("%s: a read-only property does not need a default", p.Name)
		}
		if p.None != NoneInvalid && p.Kind != KindSize && p.Kind != KindNumber {
			t.Errorf("%s: \"none\" only applies to numbers", p.Name)
		}
		if p.Kind == KindList && !p.ReadOnly {
			t.Errorf("%s: lists are read-only", p.Name)
		}
	}
	for _, name := range []string{"compression", "checksum", "atime", "quota", "mountpoint", "creation", "compressratio", "type", "used"} {
		if _, ok := LookupProp(name); !ok {
			t.Errorf("%s is missing from the table", name)
		}
	}
}

func TestLookupProp(t *testing.T) {
	for name, want := range map[string]struct {
		kind PropKind
		ro   bool
	}{
		"compression":      {KindEnum, false},
		"used":             {KindSize, true},
		"written@s1":       {KindSize, true},
		"written#bm":       {KindSize, true},
		"userused@root":    {KindSize, true},
		"userquota@root":   {KindSize, false},
		"test:note":        {KindUser, false},
		"org.freebsd:swap": {KindUser, false},
	} {
		info, ok := LookupProp(name)
		if !ok || info.Kind != want.kind || info.ReadOnly != want.ro || info.Name != name {
			t.Errorf("LookupProp(%q) = %+v, %v; want kind %v readonly %v", name, info, ok, want.kind, want.ro)
		}
	}
	for _, name := range []string{"", "bogus", "written@", "userused@", "Bad Name:x", "tab\t:x", strings.Repeat("a", 300) + ":x"} {
		if info, ok := LookupProp(name); ok {
			t.Errorf("LookupProp(%q) = %+v, must be unknown", name, info)
		}
	}
	if !IsUserProp("a:b") || IsUserProp("ab") || IsUserProp("a b:c") {
		t.Error("IsUserProp")
	}
}

func TestTypedGetters(t *testing.T) {
	clones := nvlist.New()
	clones.Add("p/c2", nvlist.Flag{})
	clones.Add("p/c1", nvlist.Flag{})
	d := &Dataset{Name: "p/a", Type: Volume, Props: map[string]Property{
		"compression":   {Value: uint64(15), Source: "p"},
		"checksum":      {Value: uint64(99)}, // a number the table does not know
		"atime":         {Value: uint64(1)},
		"readonly":      {Value: uint64(0), Source: "p/a"},
		"mounted":       {Value: uint64(1)},
		"type":          {Value: uint64(3)},
		"creation":      {Value: uint64(1700000000)},
		"compressratio": {Value: uint64(150)},
		"used":          {Value: uint64(4096)},
		"mountpoint":    {Value: "/x", Source: "p/a"},
		"test:note":     {Value: "hi", Source: "p/a"},
		"clones":        {Value: clones},
		"mystery":       {Value: uint64(7)},
	}}

	if s, ok := d.Enum("compression"); !ok || s != "lz4" {
		t.Errorf("Enum(compression) = %q, %v", s, ok)
	}
	if s, ok := d.Enum("atime"); !ok || s != "on" {
		t.Errorf("Enum(atime) = %q, %v", s, ok)
	}
	if s, ok := d.Enum("mounted"); !ok || s != "yes" {
		t.Errorf("Enum(mounted) = %q, %v", s, ok)
	}
	if s, ok := d.Enum("type"); !ok || s != "volume" {
		t.Errorf("Enum(type) = %q, %v", s, ok)
	}
	for _, name := range []string{"checksum", "used", "nope", "mountpoint", "mystery"} {
		if s, ok := d.Enum(name); ok {
			t.Errorf("Enum(%s) = %q, want not ok", name, s)
		}
	}
	if v, ok := d.Bool("atime"); !ok || !v {
		t.Error("Bool(atime)")
	}
	if v, ok := d.Bool("readonly"); !ok || v {
		t.Error("Bool(readonly)")
	}
	if v, ok := d.Bool("mounted"); !ok || !v {
		t.Error("Bool(mounted)")
	}
	if _, ok := d.Bool("compression"); ok {
		t.Error("an enumeration is not a bool")
	}
	if tm, ok := d.Time("creation"); !ok || !tm.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("Time = %v, %v", tm, ok)
	}
	if _, ok := d.Time("used"); ok {
		t.Error("used is not a time")
	}
	if r, ok := d.Ratio("compressratio"); !ok || r != 1.5 {
		t.Errorf("Ratio = %v, %v", r, ok)
	}

	for name, want := range map[string]any{
		"compression":   "lz4",
		"checksum":      uint64(99), // unknown number: the raw value is better than nothing
		"atime":         true,
		"readonly":      false,
		"type":          "volume",
		"creation":      time.Unix(1700000000, 0),
		"compressratio": 1.5,
		"used":          uint64(4096),
		"mountpoint":    "/x",
		"test:note":     "hi",
		"clones":        []string{"p/c1", "p/c2"},
		"mystery":       uint64(7), // not in the table: as the kernel gave it
	} {
		got, ok := d.Value(name)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("Value(%s) = %#v, %v; want %#v", name, got, ok, want)
		}
	}
	if _, ok := d.Value("absent"); ok {
		t.Error("Value of an absent property")
	}

	for name, want := range map[string]Source{
		"compression": {Kind: SourceInherited, From: "p"},
		"readonly":    {Kind: SourceLocal},
		"used":        {Kind: SourceDefault},
	} {
		if got, ok := d.Source(name); !ok || got != want {
			t.Errorf("Source(%s) = %+v, %v; want %+v", name, got, ok, want)
		}
	}
	r := &Dataset{Name: "p/a", Props: map[string]Property{"x:y": {Value: "v", Source: "$recvd"}}}
	if s, _ := r.Source("x:y"); s.Kind != SourceReceived || s.String() != "received" {
		t.Errorf("received: %+v", s)
	}
	if s := (Source{Kind: SourceInherited, From: "p"}); s.String() != "inherited from p" {
		t.Errorf("String = %q", s)
	}
	if info, ok := d.Info("compression"); !ok || info.Kind != KindEnum {
		t.Errorf("Info: %+v", info)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{
		"0": 0, "512": 512, "64k": 64 << 10, "64K": 64 << 10, "64KB": 64 << 10, "64KiB": 64 << 10, "64kib": 64 << 10,
		"10M": 10 << 20, "1G": 1 << 30, "1.5G": 3 << 29, "0.5K": 512, "2T": 2 << 40, "1P": 1 << 50, "1E": 1 << 60,
		" 10G ": 10 << 30, "10 G": 10 << 30, "100B": 100, "15E": 15 << 60,
	} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for in, why := range map[string]string{
		"": "empty", "abc": "no number", "-1": "negative", "1.1K": "fraction of a byte", "1..2": "two dots",
		".5G": "leading dot", "1.": "trailing dot", "1X": "unknown unit", "16E": "overflow", "99999999999E": "overflow",
		"1e3": "exponent", "0x10": "hex", "1KK": "bad unit",
	} {
		if got, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) = %d: expected an error (%s)", in, got, why)
		}
	}
}

func TestConvertProp(t *testing.T) {
	max := uint64(math.MaxUint64)
	for _, c := range []struct {
		name string
		in   any
		snap bool
		want any
	}{
		// switches
		{"atime", false, false, uint64(0)}, {"atime", true, false, uint64(1)},
		{"atime", "on", false, uint64(1)}, {"atime", "off", false, uint64(0)},
		// enumerations: by name, or by raw number
		{"compression", "lz4", false, uint64(15)}, {"compression", "off", false, uint64(2)},
		{"compression", "gzip", false, uint64(10)}, {"compression", uint64(99), false, uint64(99)},
		{"compression", 15, false, uint64(15)}, {"copies", 2, false, uint64(2)}, {"copies", "3", false, uint64(3)},
		{"sync", "always", false, uint64(1)}, {"dnodesize", "4k", false, uint64(4096)},
		// sizes
		{"quota", "10G", false, uint64(10 << 30)}, {"quota", "none", false, uint64(0)},
		{"quota", uint64(1024), false, uint64(1024)}, {"quota", 4096, false, uint64(4096)},
		{"refreservation", "none", false, uint64(0)}, {"recordsize", "64k", false, uint64(65536)},
		{"volsize", "1.5G", false, uint64(3 << 29)},
		// numbers
		{"recordsize", 512, false, uint64(512)}, {"recordsize", "16M", false, uint64(16 << 20)},
		{"special_small_blocks", 0, false, uint64(0)}, {"special_small_blocks", "32k", false, uint64(32768)},
		{"snapshot_limit", "none", false, max}, {"snapshot_limit", "5", false, uint64(5)},
		{"filesystem_limit", 10, false, uint64(10)},
		// strings and user properties
		{"mountpoint", "/data", false, "/data"}, {"mountpoint", "none", false, "none"},
		{"test:note", "hello", false, "hello"}, {"test:note", "hello", true, "hello"}, // user props work on snapshots
	} {
		got, err := convertProp(c.name, c.in, c.snap)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("convertProp(%s, %#v) = %#v, %v; want %#v", c.name, c.in, got, err, c.want)
		}
	}

	for _, c := range []struct {
		name string
		in   any
		snap bool
		want error
	}{
		{"bogus", 1, false, ErrUnknownProp},
		{"used", 1, false, ErrReadOnlyProp},
		{"written@s1", 1, false, ErrReadOnlyProp},
		{"creation", 1, false, ErrReadOnlyProp},
		{"clones", "x", false, ErrReadOnlyProp},
		{"utf8only", true, false, ErrCreateOnly},
		{"volblocksize", "8k", false, ErrCreateOnly},
		{"compression", "lz4", true, ErrReadOnlyProp}, // not a user property, on a snapshot
		{"userquota@alice", "1G", false, ErrUnsupportedProp},
		{"atime", "maybe", false, ErrBadValue}, {"atime", 1, false, ErrBadValue},
		{"compression", "bogus", false, ErrBadValue}, {"compression", -1, false, ErrBadValue},
		{"compression", true, false, ErrBadValue}, {"compression", 1.5, false, ErrBadValue},
		{"quota", -5, false, ErrBadValue}, {"quota", 1.5, false, ErrBadValue}, {"quota", "abc", false, ErrBadValue},
		{"recordsize", "none", false, ErrBadValue}, // none is only accepted where it means something
		// libzfs checks block sizes in userland; the kernel accepts any number.
		{"recordsize", 3000, false, ErrBadValue}, {"recordsize", 0, false, ErrBadValue},
		{"recordsize", 256, false, ErrBadValue}, {"recordsize", "32M", false, ErrBadValue},
		{"special_small_blocks", "32M", false, ErrBadValue},
		{"snapshot_limit", "many", false, ErrBadValue},
		{"mountpoint", 5, false, ErrBadValue},
		{"test:note", 5, false, ErrBadValue},
		{"test:note", strings.Repeat("x", 8193), false, ErrBadValue},
	} {
		got, err := convertProp(c.name, c.in, c.snap)
		if !errors.Is(err, c.want) {
			t.Errorf("convertProp(%s, %#v) = %#v, %v; want %v", c.name, c.in, got, err, c.want)
		}
		if err != nil && !strings.Contains(err.Error(), c.name) {
			t.Errorf("the error does not name the property: %v", err)
		}
	}

	// The message for a wrong enum value lists the choices.
	_, err := convertProp("sync", "sometimes", false)
	if err == nil || !strings.Contains(err.Error(), "standard, always, disabled") {
		t.Errorf("message = %v", err)
	}
}

func TestSetPropsSendsKernelValues(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	err := c.SetProps("tank/a", map[string]any{
		"compression": "lz4", "atime": false, "quota": "10G", "recordsize": "64k",
		"test:x": "y", "copies": 2, "mountpoint": "/mnt/a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(k.sets) != 1 || k.sets[0].dataset != "tank/a" {
		t.Fatalf("calls: %+v", k.sets)
	}
	want := map[string]any{
		"compression": uint64(15), "atime": uint64(0), "quota": uint64(10 << 30), "recordsize": uint64(65536),
		"test:x": "y", "copies": uint64(2), "mountpoint": "/mnt/a",
	}
	got := map[string]any{}
	for _, p := range k.sets[0].props.Pairs() {
		got[p.Name] = p.Value
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wire values = %#v\nwant        %#v", got, want)
	}

	if err := c.SetProp("tank/a", "sync", "disabled"); err != nil || len(k.sets) != 2 {
		t.Fatalf("SetProp: %v", err)
	}
	if v, _ := k.sets[1].props.Uint64("sync"); v != 2 {
		t.Errorf("sync = %d", v)
	}
	if err := c.SetProps("tank/a", nil); err != nil || len(k.sets) != 2 {
		t.Error("an empty set sends nothing")
	}
}

func TestSetPropsValidatesEverythingBeforeSending(t *testing.T) {
	k := newFake(t)
	err := newClient(k).SetProps("tank/a", map[string]any{"compression": "lz4", "atime": "maybe"})
	if !errors.Is(err, ErrBadValue) || len(k.sets) != 0 {
		t.Errorf("err %v, %d calls sent: nothing may be sent if one value is wrong", err, len(k.sets))
	}
	if err := newClient(k).SetProps("tank/a@s1", map[string]any{"compression": "lz4"}); !errors.Is(err, ErrReadOnlyProp) || len(k.sets) != 0 {
		t.Errorf("snapshot: %v", err)
	}
	if err := newClient(k).SetProps("tank/a@s1", map[string]any{"test:keep": "yes"}); err != nil {
		t.Errorf("a user property on a snapshot: %v", err)
	}
}

func TestSetPropsReportsEachRefusedProperty(t *testing.T) {
	k := newFake(t)
	k.setFail = map[string]syscall.Errno{"recordsize": syscall.EINVAL, "quota": syscall.EDQUOT}
	err := newClient(k).SetProps("tank/a", map[string]any{"recordsize": 65536, "quota": "1G", "atime": true})
	var spe *SetPropsError
	if !errors.As(err, &spe) {
		t.Fatalf("err = %#v", err)
	}
	if spe.Dataset != "tank/a" || spe.Errors["recordsize"] != syscall.EINVAL || spe.Errors["quota"] != syscall.EDQUOT || len(spe.Errors) != 2 {
		t.Errorf("%+v", spe)
	}
	if !errors.Is(err, syscall.EINVAL) || !errors.Is(err, syscall.EDQUOT) || errors.Is(err, syscall.EPERM) {
		t.Error("Unwrap must expose every errno")
	}
	want := fmt.Sprintf("zfs: cannot set properties of tank/a: quota: %v; recordsize: %v", syscall.EDQUOT, syscall.EINVAL)
	if got := err.Error(); got != want {
		t.Errorf("message = %q\nwant      %q", got, want)
	}
}

func TestSetPropsOtherErrors(t *testing.T) {
	c := newClient(newFake(t))
	if err := c.SetProps("tank/missing", map[string]any{"atime": true}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing dataset: %v", err)
	}
	k := newFake(t)
	k.hook = func(ioc ioctl.IOC, cmd *ioctl.Cmd, n int) error {
		return &ioctl.Error{Ioc: ioc, Errno: syscall.EPERM}
	}
	err := newClient(k).SetProps("tank/a", map[string]any{"atime": true})
	var spe *SetPropsError
	if !errors.Is(err, syscall.EPERM) || errors.As(err, &spe) {
		t.Errorf("an error without a detail list stays a plain ioctl error: %#v", err)
	}
}

func TestInheritProp(t *testing.T) {
	k := newFake(t)
	c := newClient(k)
	for _, prop := range []string{"compression", "test:x", "mountpoint"} {
		if err := c.InheritProp("tank/a", prop); err != nil {
			t.Errorf("%s: %v", prop, err)
		}
	}
	if len(k.inherits) != 3 || k.inherits[0] != (inheritCall{"tank/a", "compression"}) {
		t.Errorf("calls: %+v", k.inherits)
	}
	before := len(k.inherits)
	for prop, want := range map[string]error{
		"bogus": ErrUnknownProp, "used": ErrReadOnlyProp, "quota": ErrNotInheritable, "volsize": ErrNotInheritable,
	} {
		if err := c.InheritProp("tank/a", prop); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", prop, err, want)
		}
	}
	if len(k.inherits) != before {
		t.Error("nothing may be sent for a property that cannot be inherited")
	}
	if err := c.InheritProp("tank/missing", "compression"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing dataset: %v", err)
	}
}

// The kernel leaves out properties that have their default; the typed
// getters fill them in from the table.
func TestDefaultsFillInForPropertiesTheKernelDidNotSend(t *testing.T) {
	d := &Dataset{Name: "p/a", Type: Filesystem, Props: map[string]Property{
		"compression": {Value: uint64(15), Source: "p/a"}, // set: not the default
		"used":        {Value: uint64(1)},
	}}
	for name, want := range map[string]any{
		"atime":          true,  // default on
		"relatime":       true,  // default on (FreeBSD)
		"compression":    "lz4", // the stored value wins
		"checksum":       "on",  // default
		"sync":           "standard",
		"recordsize":     uint64(131072),
		"quota":          uint64(0),
		"sharenfs":       "off",
		"snapshot_limit": uint64(math.MaxUint64),
	} {
		got, ok := d.Value(name)
		if !ok || got != want {
			t.Errorf("Value(%s) = %#v, %v; want %#v", name, got, ok, want)
		}
	}
	if v, ok := d.Bool("atime"); !ok || !v {
		t.Errorf("Bool(atime) = %v, %v", v, ok)
	}
	if s, ok := d.Enum("dnodesize"); !ok || s != "legacy" {
		t.Errorf("Enum(dnodesize) = %q, %v", s, ok)
	}
	if src, ok := d.Source("atime"); !ok || src.Kind != SourceDefault {
		t.Errorf("Source(atime) = %+v, %v: a defaulted property comes from the default", src, ok)
	}
	if src, _ := d.Source("compression"); src.Kind != SourceLocal {
		t.Errorf("Source(compression) = %+v", src)
	}
	// The raw accessors are the kernel's view: defaults are not invented there.
	if _, ok := d.Uint64("atime"); ok {
		t.Error("Uint64 must only return what the kernel sent")
	}
	// Absent and without a default (or unknown): nothing.
	for _, name := range []string{"creation", "mountpoint", "bogus", "test:user", "xattr"} {
		if v, ok := d.Value(name); ok {
			t.Errorf("Value(%s) = %#v, want not ok", name, v)
		}
		if _, ok := d.Source(name); ok {
			t.Errorf("Source(%s) must not exist", name)
		}
	}
}

// The "type" property is stored as the object set type, which is the same
// for a snapshot as for its filesystem, so it is answered from the dataset.
func TestTypeComesFromTheDataset(t *testing.T) {
	for typ, want := range map[Type]string{
		Filesystem: "filesystem", Volume: "volume", Snapshot: "snapshot", Bookmark: "bookmark",
	} {
		d := &Dataset{Name: "x", Type: typ, Props: map[string]Property{"type": {Value: uint64(2)}}}
		if got, ok := d.Enum("type"); !ok || got != want {
			t.Errorf("Enum(type) of a %v = %q, %v", typ, got, ok)
		}
		if got, _ := d.Value("type"); got != want {
			t.Errorf("Value(type) of a %v = %v", typ, got)
		}
	}
	if _, ok := (&Dataset{Name: "x"}).Enum("type"); ok {
		t.Error("a dataset of unknown type has no type name")
	}
}
