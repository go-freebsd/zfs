package ioctl

import (
	"errors"
	"io/fs"
	"reflect"
	"syscall"
	"testing"
	"unsafe"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
)

// The offsets are the C layout of zfs_cmd_t on LP64, derived field by field
// from <sys/zfs_ioctl.h>. The total must equal the header's own assertion,
// _expected_zfs_cmd_size = MAXPATHLEN*3 + MAXNAMELEN + 1200.
func TestCmdLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("layout is asserted for 64-bit targets")
	}
	var c Cmd
	for name, want := range map[string][2]uintptr{
		"Name":            {unsafe.Offsetof(c.Name), 0},
		"NvlistSrc":       {unsafe.Offsetof(c.NvlistSrc), 1024},
		"NvlistSrcSize":   {unsafe.Offsetof(c.NvlistSrcSize), 1032},
		"NvlistDst":       {unsafe.Offsetof(c.NvlistDst), 1040},
		"NvlistDstSize":   {unsafe.Offsetof(c.NvlistDstSize), 1048},
		"NvlistDstFilled": {unsafe.Offsetof(c.NvlistDstFilled), 1056},
		"History":         {unsafe.Offsetof(c.History), 1064},
		"Value":           {unsafe.Offsetof(c.Value), 1072},
		"Str":             {unsafe.Offsetof(c.Str), 3120},
		"GUID":            {unsafe.Offsetof(c.GUID), 3376},
		"NvlistConf":      {unsafe.Offsetof(c.NvlistConf), 3384},
		"NvlistConfSize":  {unsafe.Offsetof(c.NvlistConfSize), 3392},
		"Cookie":          {unsafe.Offsetof(c.Cookie), 3400},
		"ObjsetType":      {unsafe.Offsetof(c.ObjsetType), 3408},
		"PermAction":      {unsafe.Offsetof(c.PermAction), 3416},
		"HistoryLen":      {unsafe.Offsetof(c.HistoryLen), 3424},
		"HistoryOffset":   {unsafe.Offsetof(c.HistoryOffset), 3432},
		"Obj":             {unsafe.Offsetof(c.Obj), 3440},
		"IFlags":          {unsafe.Offsetof(c.IFlags), 3448},
		"Share":           {unsafe.Offsetof(c.Share), 3456},
		"ObjsetStats":     {unsafe.Offsetof(c.ObjsetStats), 3488},
		"BeginRecord":     {unsafe.Offsetof(c.BeginRecord), 3776},
		"union":           {unsafe.Offsetof(c.union), 4080},
		"CleanupFD":       {unsafe.Offsetof(c.CleanupFD), 4448},
		"Simple":          {unsafe.Offsetof(c.Simple), 4452},
		"SendObj":         {unsafe.Offsetof(c.SendObj), 4456},
		"FromObj":         {unsafe.Offsetof(c.FromObj), 4464},
		"CreateTxg":       {unsafe.Offsetof(c.CreateTxg), 4472},
		"Stat":            {unsafe.Offsetof(c.Stat), 4480},
		"ZoneID":          {unsafe.Offsetof(c.ZoneID), 4520},
	} {
		if want[0] != want[1] {
			t.Errorf("offsetof %s = %d, want %d", name, want[0], want[1])
		}
	}
	for name, s := range map[string][2]uintptr{
		"Cmd":         {unsafe.Sizeof(c), CmdSize},
		"Share":       {unsafe.Sizeof(Share{}), 32},
		"ObjsetStats": {unsafe.Sizeof(ObjsetStats{}), 288},
		"DrrBegin":    {unsafe.Sizeof(DrrBegin{}), 304},
		"Stat":        {unsafe.Sizeof(Stat{}), 40},
		"iocparm":     {unsafe.Sizeof(iocparm{}), 24},
	} {
		if s[0] != s[1] {
			t.Errorf("sizeof %s = %d, want %d", name, s[0], s[1])
		}
	}
	if CmdSize != 4528 {
		t.Errorf("CmdSize = %d, want 4528", CmdSize)
	}
	// dmu_replay_record_t = type, payloadlen + the 304 byte drr_begin.
	if ReplayRecordSize != 8+unsafe.Sizeof(DrrBegin{}) {
		t.Errorf("ReplayRecordSize = %d", ReplayRecordSize)
	}
}

func TestIocNumber(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("request numbers are asserted for 64-bit targets")
	}
	// _IOWR('Z', n, struct of 24 bytes) = 0xC0185A00 | n
	for ioc, want := range map[IOC]uintptr{
		IOCPoolCreate:  0xC0185A00,
		IOCObjsetStats: 0xC0185A12,
		IOCSnapshot:    0xC0185A23,
		IOCJail:        0xC0185A85,
	} {
		if got := iocNumber(ioc); got != want {
			t.Errorf("iocNumber(%v) = %#x, want %#x", ioc, got, want)
		}
	}
}

func TestIOCTable(t *testing.T) {
	for ioc, want := range map[IOC]uint32{
		IOCPoolCreate: 0, IOCPoolConfigs: 4, IOCPoolStats: 5, IOCObjsetStats: 18,
		IOCDatasetListNext: 20, IOCSnapshotListNext: 21, IOCSetProp: 22, IOCCreate: 23,
		IOCDestroy: 24, IOCRollback: 25, IOCRename: 26, IOCRecv: 27, IOCSend: 28,
		IOCPromote: 34, IOCSnapshot: 35, IOCHold: 48, IOCRelease: 49, IOCGetHolds: 50,
		IOCDestroySnaps: 59, IOCSendNew: 64, IOCSendSpace: 65, IOCClone: 66,
		IOCBookmark: 67, IOCGetBookmarks: 68, IOCDestroyBookmarks: 69, IOCRecvNew: 70,
		IOCPoolSync: 71, IOCPoolCondense: 90, IOCPlatform: 0x80, IOCNextboot: 0x84,
		IOCJail: 0x85, IOCUnjail: 0x86, IOCGetBootenv: 0x88,
	} {
		if uint32(ioc) != want {
			t.Errorf("%v = %d, want %d", ioc, ioc, want)
		}
	}
	if IOCSnapshot.String() != "ZFS_IOC_SNAPSHOT" || IOC(200).String() != "ZFS_IOC_200" {
		t.Errorf("String: %q %q", IOCSnapshot, IOC(200))
	}
	all := AllIOCs()
	if len(all) != 91+9 { // core block + platform block
		t.Errorf("AllIOCs = %d entries, want 100", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i] <= all[i-1] {
			t.Fatalf("AllIOCs not sorted at %d", i)
		}
	}
}

func TestCStrings(t *testing.T) {
	c := NewCmd()
	if err := c.SetName("tank/fs"); err != nil || c.GetName() != "tank/fs" {
		t.Fatalf("name = %q, %v", c.GetName(), err)
	}
	if err := c.SetName("tank"); err != nil || c.GetName() != "tank" {
		t.Errorf("shorter name must clear the tail: %q", c.GetName())
	}
	long := make([]byte, 1024)
	for i := range long {
		long[i] = 'a'
	}
	if err := c.SetName(string(long)); err == nil {
		t.Error("a name without room for the NUL must be rejected")
	}
	if err := c.SetValue("v"); err != nil || c.GetValue() != "v" {
		t.Error("value")
	}
	if err := c.SetString("s"); err != nil || c.GetString() != "s" {
		t.Error("string")
	}
}

// --- Conn.Do / Call against a fake kernel ---

func packed(t *testing.T, l *nvlist.List) []byte {
	t.Helper()
	b, err := nvlist.Pack(l)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func reply(t *testing.T, cmd *Cmd, b *buffers, l *nvlist.List) {
	t.Helper()
	out := packed(t, l)
	copy(b.dst, out)
	cmd.NvlistDstSize, cmd.NvlistDstFilled = uint64(len(out)), 1
}

func listOf(t *testing.T, k, v string) *nvlist.List {
	t.Helper()
	l := nvlist.New()
	if err := l.Add(k, v); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestCallPassesNameAndInputAndReturnsOutput(t *testing.T) {
	in := listOf(t, "origin", "tank/a@s")
	want := listOf(t, "result", "ok")
	calls := 0
	c := newConn(func(ioc IOC, cmd *Cmd, b *buffers) error {
		calls++
		if ioc != IOCClone || cmd.GetName() != "tank/b" {
			t.Errorf("ioc %v name %q", ioc, cmd.GetName())
		}
		got, err := nvlist.Unpack(b.src)
		if err != nil || !reflect.DeepEqual(got, in) {
			t.Errorf("source list = %v, %v", got, err)
		}
		if cmd.NvlistSrcSize != uint64(len(b.src)) || cmd.NvlistSrc != ptr(b.src) {
			t.Error("source pointer/size not set")
		}
		if cmd.NvlistDstSize != minDstSize || len(b.dst) != minDstSize {
			t.Errorf("dst size %d, want %d", cmd.NvlistDstSize, minDstSize)
		}
		reply(t, cmd, b, want)
		return nil
	})
	out, err := c.Call(IOCClone, "tank/b", in)
	if err != nil || !reflect.DeepEqual(out, want) || calls != 1 {
		t.Fatalf("out = %v, err = %v, calls = %d", out, err, calls)
	}
}

func TestCallWithoutOutput(t *testing.T) {
	c := newConn(func(ioc IOC, cmd *Cmd, b *buffers) error {
		if cmd.NvlistSrc != 0 || cmd.NvlistSrcSize != 0 {
			t.Error("no input list means no source buffer")
		}
		return nil // NvlistDstFilled stays 0
	})
	out, err := c.Call(IOCObjsetStats, "tank", nil)
	if err != nil || out != nil {
		t.Errorf("out = %v, err = %v; want nil, nil", out, err)
	}
}

// FreeBSD reports a too small buffer as success with a larger required
// size, not as an error.
func TestCallGrowsWhenKernelAsksForMore(t *testing.T) {
	want := listOf(t, "k", "v")
	var sizes []int
	c := newConn(func(ioc IOC, cmd *Cmd, b *buffers) error {
		sizes = append(sizes, len(b.dst))
		if len(b.dst) < 600_000 {
			cmd.NvlistDstSize = 600_000
			return nil
		}
		reply(t, cmd, b, want)
		return nil
	})
	out, err := c.Call(IOCPoolStats, "tank", nil)
	if err != nil || !reflect.DeepEqual(out, want) {
		t.Fatalf("out = %v, err = %v", out, err)
	}
	if want := []int{minDstSize, 600_000}; !reflect.DeepEqual(sizes, want) {
		t.Errorf("buffer sizes = %v, want %v", sizes, want)
	}
}

func TestCallDoublesOnENOMEM(t *testing.T) {
	want := listOf(t, "k", "v")
	var sizes []int
	c := newConn(func(ioc IOC, cmd *Cmd, b *buffers) error {
		sizes = append(sizes, len(b.dst))
		if len(sizes) < 3 {
			return syscall.ENOMEM
		}
		reply(t, cmd, b, want)
		return nil
	})
	if _, err := c.Call(IOCPoolStats, "tank", nil); err != nil {
		t.Fatal(err)
	}
	if want := []int{minDstSize, 2 * minDstSize, 4 * minDstSize}; !reflect.DeepEqual(sizes, want) {
		t.Errorf("buffer sizes = %v, want %v", sizes, want)
	}
}

func TestCallGivesUpAtTheLimit(t *testing.T) {
	old := maxDstSize
	maxDstSize = 1 << 20
	t.Cleanup(func() { maxDstSize = old })
	calls := 0
	c := newConn(func(ioc IOC, cmd *Cmd, b *buffers) error { calls++; return syscall.ENOMEM })
	_, err := c.Call(IOCPoolStats, "tank", nil)
	var e *Error
	if !errors.As(err, &e) || e.Errno != syscall.ENOMEM {
		t.Fatalf("err = %v, want ENOMEM *Error", err)
	}
	if calls > 5 {
		t.Errorf("%d attempts for a 1 MiB limit", calls)
	}
}

func TestErrorMapping(t *testing.T) {
	c := newConn(func(IOC, *Cmd, *buffers) error { return syscall.ENOENT })
	_, err := c.Call(IOCObjsetStats, "tank/missing", nil)
	var e *Error
	if !errors.As(err, &e) || e.Ioc != IOCObjsetStats || e.Name != "tank/missing" || e.Errno != syscall.ENOENT {
		t.Fatalf("err = %#v", err)
	}
	if !errors.Is(err, fs.ErrNotExist) || !errors.Is(err, syscall.ENOENT) {
		t.Error("must unwrap to the errno")
	}
	if got, want := err.Error(), `ZFS_IOC_OBJSET_STATS "tank/missing": no such file or directory`; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}

	c = newConn(func(IOC, *Cmd, *buffers) error { return syscall.EEXIST })
	if _, err := c.Call(IOCCreate, "tank/x", nil); !errors.Is(err, fs.ErrExist) {
		t.Errorf("EEXIST: %v", err)
	}
}

func TestIoctlLegacyKeepsFieldsAndClearsBuffers(t *testing.T) {
	c := newConn(func(ioc IOC, cmd *Cmd, b *buffers) error {
		if cmd.Cookie != 42 || cmd.GetValue() != "tank/fs@a" {
			t.Errorf("legacy fields lost: cookie %d value %q", cmd.Cookie, cmd.GetValue())
		}
		if cmd.NvlistSrc|cmd.NvlistDst != 0 {
			t.Error("nvlist pointers must be cleared")
		}
		cmd.Cookie = 43
		return nil
	})
	cmd := NewCmd()
	cmd.Cookie = 42
	cmd.NvlistSrc, cmd.NvlistDst = 1, 2
	cmd.SetValue("tank/fs@a")
	if err := c.Ioctl(IOCRollback, cmd); err != nil || cmd.Cookie != 43 {
		t.Errorf("err = %v, cookie = %d", err, cmd.Cookie)
	}
	c = newConn(func(IOC, *Cmd, *buffers) error { return syscall.EBUSY })
	if err := c.Ioctl(IOCRollback, NewCmd()); !errors.Is(err, syscall.EBUSY) {
		t.Errorf("err = %v", err)
	}
}

func TestCloseOnce(t *testing.T) {
	n := 0
	c := newConn(nil)
	c.fd = 7
	c.closer = func(fd int) error { n++; return nil }
	if c.Close() != nil || c.Close() != nil || n != 1 {
		t.Errorf("closer ran %d times, want 1", n)
	}
	if c.FD() != 7 {
		t.Errorf("FD = %d", c.FD())
	}
}

func TestOpenUnsupportedOffFreeBSD(t *testing.T) {
	if _, err := Open(); err == nil {
		return // running on FreeBSD with ZFS: Open worked
	} else if !errors.Is(err, ErrUnsupported) && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission) {
		t.Errorf("Open: %v", err)
	}
}

func TestTraceSeesPackedLists(t *testing.T) {
	in := listOf(t, "a", "b")
	out := listOf(t, "c", "d")
	var gotSrc, gotDst []byte
	var gotName string
	c := newConn(func(ioc IOC, cmd *Cmd, b *buffers) error { reply(t, cmd, b, out); return nil })
	c.Trace = func(ioc IOC, name string, src, dst []byte) { gotName, gotSrc, gotDst = name, src, dst }
	if _, err := c.Call(IOCSnapshot, "tank", in); err != nil {
		t.Fatal(err)
	}
	if gotName != "tank" || !reflect.DeepEqual(gotSrc, packed(t, in)) || !reflect.DeepEqual(gotDst, packed(t, out)) {
		t.Errorf("trace saw %q %x %x", gotName, gotSrc, gotDst)
	}
}
