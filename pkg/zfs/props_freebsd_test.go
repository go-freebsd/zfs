//go:build freebsd

package zfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"
)

// cliProps returns "zfs get all" of a dataset as property -> value.
func cliProps(t *testing.T, name string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, r := range rows(cli(t, "zfs", "get", "-H", "-o", "property,value", "all", name)) {
		if len(r) == 2 {
			m[r[0]] = r[1]
		}
	}
	return m
}

func fixtureDatasets(pool string) []string {
	return []string{pool, pool + "/a", pool + "/a/b", pool + "/a/b/c", pool + "/vol", pool + "/clone",
		pool + "/a@s1", pool + "/a/b@r1", pool + "/vol@v1"}
}

// Every property the kernel returns must be in the table, or typed access
// would silently fall back to raw numbers for it.
func TestPropertyTableCoversWhatTheKernelReturns(t *testing.T) {
	c, pool := testClient(t)
	unknown := map[string]string{}
	for _, name := range fixtureDatasets(pool) {
		d, err := c.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		for prop, p := range d.Props {
			if _, ok := LookupProp(prop); !ok {
				unknown[prop] = fmt.Sprintf("%T %v (on %s)", p.Value, p.Value, name)
			}
		}
	}
	var names []string
	for n := range unknown {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t.Errorf("property %q is not in the table: kernel value %s", n, unknown[n])
	}
}

// Reading: the name this library gives a stored number must be the name
// zfs get prints.
func TestEnumNamesMatchZfsGet(t *testing.T) {
	c, pool := testClient(t)
	compared := 0
	seen := map[string]bool{}
	for _, name := range fixtureDatasets(pool) {
		d, err := c.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		cl := cliProps(t, name)
		for prop := range d.Props {
			info, ok := LookupProp(prop)
			if !ok || (info.Kind != KindEnum && info.Kind != KindBool) {
				continue
			}
			want, have := cl[prop]
			if !have {
				continue
			}
			n, _ := d.Uint64(prop)
			got, known := d.Enum(prop)
			switch {
			case !known:
				t.Errorf("%s %s: kernel number %d is %q in zfs get, the table has no name for it", name, prop, n, want)
			case got != want:
				t.Errorf("%s %s: kernel number %d is %q here but %q in zfs get", name, prop, n, got, want)
			default:
				compared++
				seen[prop] = true
			}
		}
	}
	t.Logf("%d enumerated values compared over %d properties", compared, len(seen))
	if compared < 40 || len(seen) < 10 {
		t.Errorf("only %d values / %d properties compared: the test would pass vacuously", compared, len(seen))
	}
}

// scratch creates a filesystem and a volume for tests that change things.
func scratch(t *testing.T, pool string) (fsName, volName string) {
	t.Helper()
	fsName, volName = pool+"/scratch_fs", pool+"/scratch_vol"
	cli(t, "zfs", "create", "-o", "mountpoint=none", fsName)
	cli(t, "zfs", "create", "-V", "1m", volName)
	t.Cleanup(func() {
		exec.Command("zfs", "destroy", "-rf", fsName).Run()
		exec.Command("zfs", "destroy", "-rf", volName).Run()
	})
	return
}

// Writing: set every value through zfs(8) and check the number the kernel
// stores is the one in the table.
func TestEnumValuesMatchTheKernel(t *testing.T) {
	c, pool := testClient(t)
	fsName, volName := scratch(t, pool)

	verified, refused := 0, 0
	props := map[string]bool{}
	for _, info := range propTable {
		if (info.Kind != KindEnum && info.Kind != KindBool) || info.ReadOnly || info.CreateOnly {
			continue
		}
		target := fsName
		if info.Types&Filesystem == 0 {
			target = volName
		}
		for _, v := range info.Values {
			out, err := exec.Command("zfs", "set", info.Name+"="+v.Name, target).CombinedOutput()
			if err != nil {
				refused++
				t.Logf("zfs set %s=%s: %s", info.Name, v.Name, strings.TrimSpace(string(out)))
				continue
			}
			d, err := c.Get(target)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := d.Uint64(info.Name)
			switch {
			case !ok:
				t.Errorf("%s=%s: the kernel returned no number for the property", info.Name, v.Name)
			case got != v.Num:
				t.Errorf("%s=%s: the table says %d, the kernel stores %d", info.Name, v.Name, v.Num, got)
			default:
				verified++
				props[info.Name] = true
			}
		}
	}
	t.Logf("%d values of %d properties verified, %d refused by the CLI", verified, len(props), refused)
	if verified < 40 || len(props) < 15 {
		t.Errorf("only %d values / %d properties were verified", verified, len(props))
	}
}

func TestTypedGettersOnRealDatasets(t *testing.T) {
	c, pool := testClient(t)

	b, _ := c.Get(pool + "/a/b")
	if v, ok := b.Enum("compression"); !ok || v != "lz4" {
		t.Errorf("a/b compression = %q, %v", v, ok)
	}
	if s, _ := b.Source("compression"); s.Kind != SourceLocal {
		t.Errorf("source = %v", s)
	}
	c2, _ := c.Get(pool + "/a/b/c")
	if v, _ := c2.Enum("compression"); v != "lz4" {
		t.Errorf("a/b/c inherits lz4, got %q", v)
	}
	if s, _ := c2.Source("compression"); s.Kind != SourceInherited || s.From != pool+"/a/b" {
		t.Errorf("a/b/c source = %v", s)
	}
	a, _ := c.Get(pool + "/a")
	if v, ok := a.Bool("atime"); !ok || !v {
		t.Errorf("atime = %v, %v", v, ok)
	}
	if v, ok := a.Value("quota"); !ok || v != uint64(64<<20) {
		t.Errorf("quota = %v, %v", v, ok)
	}
	if ct, ok := a.Time("creation"); !ok || time.Since(ct) > time.Hour || time.Since(ct) < 0 {
		t.Errorf("creation = %v, %v", ct, ok)
	}
	if r, ok := b.Ratio("compressratio"); !ok || r < 1 {
		t.Errorf("compressratio = %v, %v", r, ok)
	}
	if v, _ := a.Enum("type"); v != "filesystem" {
		t.Errorf("type = %q", v)
	}
	vol, _ := c.Get(pool + "/vol")
	if v, _ := vol.Enum("type"); v != "volume" {
		t.Errorf("volume type = %q", v)
	}
	snap, _ := c.Get(pool + "/a@s1")
	if v, _ := snap.Enum("type"); v != "snapshot" {
		t.Errorf("snapshot type = %q", v)
	}
	if v, ok := a.Value("test:note"); !ok || v != "hello world" {
		t.Errorf("user property = %v, %v", v, ok)
	}
	if v, ok := snap.Value("clones"); !ok || len(v.([]string)) != 1 {
		t.Errorf("clones = %v, %v", v, ok)
	}
}

func TestSetPropsAgainstZfsGet(t *testing.T) {
	c, pool := testClient(t)
	fsName, _ := scratch(t, pool)

	err := c.SetProps(fsName, map[string]any{
		"compression": "lz4", "atime": false, "quota": "10M", "recordsize": "64k",
		"copies": 2, "sync": "always", "test:x": "y z", "mountpoint": "none", "dedup": "sha256,verify",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows(cli(t, "zfs", "get", "-Hp", "-o", "property,value",
		"compression,atime,quota,recordsize,copies,sync,test:x,mountpoint,dedup", fsName)) {
		got[r[0]] = r[1]
	}
	for prop, want := range map[string]string{
		"compression": "lz4", "atime": "off", "quota": "10485760", "recordsize": "65536",
		"copies": "2", "sync": "always", "test:x": "y z", "mountpoint": "none", "dedup": "sha256,verify",
	} {
		if got[prop] != want {
			t.Errorf("zfs get %s = %q, set %q", prop, got[prop], want)
		}
	}

	// And our getters read it back with the right types.
	d, _ := c.Get(fsName)
	for prop, want := range map[string]any{
		"compression": "lz4", "atime": false, "quota": uint64(10 << 20), "recordsize": uint64(65536),
		"sync": "always", "test:x": "y z", "dedup": "sha256,verify",
	} {
		if v, _ := d.Value(prop); v != want {
			t.Errorf("Value(%s) = %#v, want %#v", prop, v, want)
		}
		if s, _ := d.Source(prop); s.Kind != SourceLocal {
			t.Errorf("source of %s = %v", prop, s)
		}
	}

	// Inherit: back to the parent's value.
	if err := c.InheritProp(fsName, "compression"); err != nil {
		t.Fatal(err)
	}
	d, _ = c.Get(fsName)
	if s, _ := d.Source("compression"); s.Kind == SourceLocal {
		t.Errorf("compression is still local after InheritProp: %v", s)
	}
	if err := c.InheritProp(fsName, "test:x"); err != nil {
		t.Fatal(err)
	}
	if out := cli(t, "zfs", "get", "-H", "-o", "value", "test:x", fsName); out != "-" {
		t.Errorf("user property after inherit = %q", out)
	}

	// "none" removes a quota.
	if err := c.SetProp(fsName, "quota", "none"); err != nil {
		t.Fatal(err)
	}
	if v := cli(t, "zfs", "get", "-Hp", "-o", "value", "quota", fsName); v != "0" && v != "none" {
		t.Errorf("quota after none = %q", v)
	}

	// A user property on a snapshot.
	cli(t, "zfs", "snapshot", fsName+"@s")
	if err := c.SetProp(fsName+"@s", "test:snap", "1"); err != nil {
		t.Errorf("user property on a snapshot: %v", err)
	}
}

func TestParseSizeMatchesZfsSet(t *testing.T) {
	c, pool := testClient(t)
	fsName, _ := scratch(t, pool)
	for _, s := range []string{"1.5G", "10M", "512k", "64K", "2T", "1T", "3.25M"} {
		cli(t, "zfs", "set", "quota="+s, fsName)
		d, err := c.Get(fsName, "quota")
		if err != nil {
			t.Fatal(err)
		}
		kernel, _ := d.Uint64("quota")
		ours, perr := ParseSize(s)
		if perr != nil || ours != kernel {
			t.Errorf("ParseSize(%q) = %d, %v; zfs set quota=%s stored %d", s, ours, perr, s, kernel)
		}
	}
}

func TestKernelRefusalsAreReportedPerProperty(t *testing.T) {
	c, pool := testClient(t)
	fsName, _ := scratch(t, pool)

	// A quota below what the dataset already uses is refused by the kernel
	// (recordsize=3000 is not: the kernel accepts it, so SetProps checks it).
	err := c.SetProps(fsName, map[string]any{"quota": 100, "atime": false})
	var spe *SetPropsError
	if !errors.As(err, &spe) {
		t.Fatalf("err = %#v, want a *SetPropsError", err)
	}
	t.Logf("kernel refusal: %v", err)
	if _, bad := spe.Errors["quota"]; !bad || spe.Dataset != fsName {
		t.Errorf("%+v", spe)
	}

	if err := c.SetProp(pool+"/does_not_exist", "atime", true); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing dataset: %v", err)
	}
	if err := c.SetProp(fsName, "recordsize", 3000); !errors.Is(err, ErrBadValue) {
		t.Errorf("recordsize 3000 must be refused before it is sent: %v", err)
	}
	// A property of volumes on a filesystem is refused by the kernel, not by the table.
	err = c.SetProp(fsName, "volsize", "1M")
	if err == nil {
		t.Error("volsize on a filesystem must fail")
	} else {
		t.Logf("volsize on a filesystem: %v", err)
	}
	// And a read-only property never reaches the kernel.
	if err := c.SetProp(fsName, "used", 1); !errors.Is(err, ErrReadOnlyProp) {
		t.Errorf("used: %v", err)
	}
}
