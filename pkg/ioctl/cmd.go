package ioctl

import (
	"errors"
	"unsafe"
)

// Sizes from <sys/zfs_ioctl.h>.
const (
	maxPathLen = 1024
	maxNameLen = 256

	// CmdSize is sizeof(zfs_cmd_t): MAXPATHLEN*3 + MAXNAMELEN + 1200.
	CmdSize = maxPathLen*3 + maxNameLen + 1200

	// DMUBackupMagic starts every send stream (DMU_BACKUP_MAGIC).
	DMUBackupMagic = 0x2F5bacbac

	// ReplayRecordSize is sizeof(dmu_replay_record_t).
	ReplayRecordSize = 312
)

// Share is zfs_share_t.
type Share struct {
	ExportData uint64
	ShareData  uint64
	ShareType  uint64
	ShareMax   uint64
}

// ObjsetStats is dmu_objset_stats_t.
type ObjsetStats struct {
	NumClones    uint64
	CreationTxg  uint64
	GUID         uint64
	Type         uint32 // dmu_objset_type_t: 2 filesystem, 3 volume
	IsSnapshot   uint8
	Inconsistent uint8
	Redacted     uint8
	Origin       [256]byte
	Flags        uint8
}

// DrrBegin is struct drr_begin, the header of a send stream.
type DrrBegin struct {
	Magic        uint64
	VersionInfo  uint64
	CreationTime uint64
	Type         uint32
	Flags        uint32
	ToGUID       uint64
	FromGUID     uint64
	ToName       [256]byte
}

// Stat is zfs_stat_t.
type Stat struct {
	Gen   uint64
	Mode  uint64
	Links uint64
	Ctime [2]uint64
}

// Cmd is zfs_cmd_t, the argument of every ZFS ioctl. The layout matches the
// C structure exactly (see TestCmdLayout). Pointer fields are plain
// integers, so a Cmd must be allocated with NewCmd (the heap), and the
// buffers they refer to must be kept alive by the caller; Conn.Do does both.
//
// The fields used by the nvlist based ioctls are Name and the Nvlist*
// members; the others serve the legacy ioctls.
type Cmd struct {
	Name            [maxPathLen]byte
	NvlistSrc       uint64
	NvlistSrcSize   uint64
	NvlistDst       uint64
	NvlistDstSize   uint64
	NvlistDstFilled int32 // boolean_t
	pad2            int32
	History         uint64
	Value           [2 * maxPathLen]byte
	Str             [maxNameLen]byte // zc_string
	GUID            uint64
	NvlistConf      uint64
	NvlistConfSize  uint64
	Cookie          uint64
	ObjsetType      uint64
	PermAction      uint64
	HistoryLen      uint64
	HistoryOffset   uint64
	Obj             uint64
	IFlags          uint64
	Share           Share
	ObjsetStats     ObjsetStats
	BeginRecord     DrrBegin
	// union { zinject_record_t zc_inject_record;
	//         struct { zc_defer_destroy, zc_flags, zc_action_handle } }
	union     [368]byte
	CleanupFD int32
	Simple    uint8
	pad       [3]uint8
	SendObj   uint64
	FromObj   uint64
	CreateTxg uint64
	Stat      Stat
	ZoneID    uint64
}

// NewCmd returns a zeroed Cmd on the heap.
func NewCmd() *Cmd { return new(Cmd) }

var errTooLong = errors.New("zfs: string too long for its zfs_cmd_t field")

func setCString(dst []byte, s string) error {
	if len(s) >= len(dst) {
		return errTooLong
	}
	n := copy(dst, s)
	for i := n; i < len(dst); i++ {
		dst[i] = 0
	}
	return nil
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// SetName sets zc_name (the pool or dataset).
func (c *Cmd) SetName(s string) error { return setCString(c.Name[:], s) }

// GetName returns zc_name.
func (c *Cmd) GetName() string { return cString(c.Name[:]) }

// SetValue sets zc_value.
func (c *Cmd) SetValue(s string) error { return setCString(c.Value[:], s) }

// GetValue returns zc_value.
func (c *Cmd) GetValue() string { return cString(c.Value[:]) }

// SetString sets zc_string.
func (c *Cmd) SetString(s string) error { return setCString(c.Str[:], s) }

// GetString returns zc_string.
func (c *Cmd) GetString() string { return cString(c.Str[:]) }

// Origin returns dds_origin of the object set statistics.
func (s *ObjsetStats) OriginName() string { return cString(s.Origin[:]) }

func (d *DrrBegin) ToNameString() string { return cString(d.ToName[:]) }

func ptr(b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}
	return uint64(uintptr(unsafe.Pointer(&b[0])))
}

// zfs_iocparm_t, the FreeBSD wrapper around zfs_cmd_t (ZFS_IOCVER_OZFS).
type iocparm struct {
	Version uint32
	_       uint32 // alignment of the 64-bit fields
	Cmd     uint64
	CmdSize uint64
}

const (
	iocVersion = 15 // ZFS_IOCVER_OZFS

	// _IOWR('Z', ioc, zfs_iocparm_t) on FreeBSD:
	// IOC_INOUT | (len & IOCPARM_MASK) << 16 | group << 8 | num.
	iocInOut    = 0xC0000000
	iocParmMask = 0x1fff
	iocGroup    = 'Z'
)

// iocNumber returns the ioctl(2) request for a ZFS ioctl.
func iocNumber(ioc IOC) uintptr {
	return uintptr(iocInOut | (uint32(unsafe.Sizeof(iocparm{}))&iocParmMask)<<16 | iocGroup<<8 | uint32(ioc))
}
