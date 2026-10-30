// Code generated from OpenZFS include/sys/fs/zfs.h (zfs_ioc_t) as of 2026-10-30; DO NOT EDIT.
// Values are FreeBSD's: ZFS_IOC_FIRST is 0 there and the platform block starts at 0x80.
// Linux numbers differ. The FreeBSD tests check a known-answer call per area.

package ioctl

// IOC is a ZFS ioctl number (zfs_ioc_t).
type IOC uint32

const (
	IOCPoolCreate            IOC = 0  // ZFS_IOC_POOL_CREATE
	IOCPoolDestroy           IOC = 1  // ZFS_IOC_POOL_DESTROY
	IOCPoolImport            IOC = 2  // ZFS_IOC_POOL_IMPORT
	IOCPoolExport            IOC = 3  // ZFS_IOC_POOL_EXPORT
	IOCPoolConfigs           IOC = 4  // ZFS_IOC_POOL_CONFIGS
	IOCPoolStats             IOC = 5  // ZFS_IOC_POOL_STATS
	IOCPoolTryimport         IOC = 6  // ZFS_IOC_POOL_TRYIMPORT
	IOCPoolScan              IOC = 7  // ZFS_IOC_POOL_SCAN
	IOCPoolFreeze            IOC = 8  // ZFS_IOC_POOL_FREEZE
	IOCPoolUpgrade           IOC = 9  // ZFS_IOC_POOL_UPGRADE
	IOCPoolGetHistory        IOC = 10 // ZFS_IOC_POOL_GET_HISTORY
	IOCVdevAdd               IOC = 11 // ZFS_IOC_VDEV_ADD
	IOCVdevRemove            IOC = 12 // ZFS_IOC_VDEV_REMOVE
	IOCVdevSetState          IOC = 13 // ZFS_IOC_VDEV_SET_STATE
	IOCVdevAttach            IOC = 14 // ZFS_IOC_VDEV_ATTACH
	IOCVdevDetach            IOC = 15 // ZFS_IOC_VDEV_DETACH
	IOCVdevSetpath           IOC = 16 // ZFS_IOC_VDEV_SETPATH
	IOCVdevSetfru            IOC = 17 // ZFS_IOC_VDEV_SETFRU
	IOCObjsetStats           IOC = 18 // ZFS_IOC_OBJSET_STATS
	IOCObjsetZplprops        IOC = 19 // ZFS_IOC_OBJSET_ZPLPROPS
	IOCDatasetListNext       IOC = 20 // ZFS_IOC_DATASET_LIST_NEXT
	IOCSnapshotListNext      IOC = 21 // ZFS_IOC_SNAPSHOT_LIST_NEXT
	IOCSetProp               IOC = 22 // ZFS_IOC_SET_PROP
	IOCCreate                IOC = 23 // ZFS_IOC_CREATE
	IOCDestroy               IOC = 24 // ZFS_IOC_DESTROY
	IOCRollback              IOC = 25 // ZFS_IOC_ROLLBACK
	IOCRename                IOC = 26 // ZFS_IOC_RENAME
	IOCRecv                  IOC = 27 // ZFS_IOC_RECV
	IOCSend                  IOC = 28 // ZFS_IOC_SEND
	IOCInjectFault           IOC = 29 // ZFS_IOC_INJECT_FAULT
	IOCClearFault            IOC = 30 // ZFS_IOC_CLEAR_FAULT
	IOCInjectListNext        IOC = 31 // ZFS_IOC_INJECT_LIST_NEXT
	IOCErrorLog              IOC = 32 // ZFS_IOC_ERROR_LOG
	IOCClear                 IOC = 33 // ZFS_IOC_CLEAR
	IOCPromote               IOC = 34 // ZFS_IOC_PROMOTE
	IOCSnapshot              IOC = 35 // ZFS_IOC_SNAPSHOT
	IOCDsobjToDsname         IOC = 36 // ZFS_IOC_DSOBJ_TO_DSNAME
	IOCObjToPath             IOC = 37 // ZFS_IOC_OBJ_TO_PATH
	IOCPoolSetProps          IOC = 38 // ZFS_IOC_POOL_SET_PROPS
	IOCPoolGetProps          IOC = 39 // ZFS_IOC_POOL_GET_PROPS
	IOCSetFsacl              IOC = 40 // ZFS_IOC_SET_FSACL
	IOCGetFsacl              IOC = 41 // ZFS_IOC_GET_FSACL
	IOCShare                 IOC = 42 // ZFS_IOC_SHARE
	IOCInheritProp           IOC = 43 // ZFS_IOC_INHERIT_PROP
	IOCSmbAcl                IOC = 44 // ZFS_IOC_SMB_ACL
	IOCUserspaceOne          IOC = 45 // ZFS_IOC_USERSPACE_ONE
	IOCUserspaceMany         IOC = 46 // ZFS_IOC_USERSPACE_MANY
	IOCUserspaceUpgrade      IOC = 47 // ZFS_IOC_USERSPACE_UPGRADE
	IOCHold                  IOC = 48 // ZFS_IOC_HOLD
	IOCRelease               IOC = 49 // ZFS_IOC_RELEASE
	IOCGetHolds              IOC = 50 // ZFS_IOC_GET_HOLDS
	IOCObjsetRecvdProps      IOC = 51 // ZFS_IOC_OBJSET_RECVD_PROPS
	IOCVdevSplit             IOC = 52 // ZFS_IOC_VDEV_SPLIT
	IOCNextObj               IOC = 53 // ZFS_IOC_NEXT_OBJ
	IOCDiff                  IOC = 54 // ZFS_IOC_DIFF
	IOCTmpSnapshot           IOC = 55 // ZFS_IOC_TMP_SNAPSHOT
	IOCObjToStats            IOC = 56 // ZFS_IOC_OBJ_TO_STATS
	IOCSpaceWritten          IOC = 57 // ZFS_IOC_SPACE_WRITTEN
	IOCSpaceSnaps            IOC = 58 // ZFS_IOC_SPACE_SNAPS
	IOCDestroySnaps          IOC = 59 // ZFS_IOC_DESTROY_SNAPS
	IOCPoolReguid            IOC = 60 // ZFS_IOC_POOL_REGUID
	IOCPoolReopen            IOC = 61 // ZFS_IOC_POOL_REOPEN
	IOCSendProgress          IOC = 62 // ZFS_IOC_SEND_PROGRESS
	IOCLogHistory            IOC = 63 // ZFS_IOC_LOG_HISTORY
	IOCSendNew               IOC = 64 // ZFS_IOC_SEND_NEW
	IOCSendSpace             IOC = 65 // ZFS_IOC_SEND_SPACE
	IOCClone                 IOC = 66 // ZFS_IOC_CLONE
	IOCBookmark              IOC = 67 // ZFS_IOC_BOOKMARK
	IOCGetBookmarks          IOC = 68 // ZFS_IOC_GET_BOOKMARKS
	IOCDestroyBookmarks      IOC = 69 // ZFS_IOC_DESTROY_BOOKMARKS
	IOCRecvNew               IOC = 70 // ZFS_IOC_RECV_NEW
	IOCPoolSync              IOC = 71 // ZFS_IOC_POOL_SYNC
	IOCChannelProgram        IOC = 72 // ZFS_IOC_CHANNEL_PROGRAM
	IOCLoadKey               IOC = 73 // ZFS_IOC_LOAD_KEY
	IOCUnloadKey             IOC = 74 // ZFS_IOC_UNLOAD_KEY
	IOCChangeKey             IOC = 75 // ZFS_IOC_CHANGE_KEY
	IOCRemap                 IOC = 76 // ZFS_IOC_REMAP
	IOCPoolCheckpoint        IOC = 77 // ZFS_IOC_POOL_CHECKPOINT
	IOCPoolDiscardCheckpoint IOC = 78 // ZFS_IOC_POOL_DISCARD_CHECKPOINT
	IOCPoolInitialize        IOC = 79 // ZFS_IOC_POOL_INITIALIZE
	IOCPoolTrim              IOC = 80 // ZFS_IOC_POOL_TRIM
	IOCRedact                IOC = 81 // ZFS_IOC_REDACT
	IOCGetBookmarkProps      IOC = 82 // ZFS_IOC_GET_BOOKMARK_PROPS
	IOCWait                  IOC = 83 // ZFS_IOC_WAIT
	IOCWaitFs                IOC = 84 // ZFS_IOC_WAIT_FS
	IOCVdevGetProps          IOC = 85 // ZFS_IOC_VDEV_GET_PROPS
	IOCVdevSetProps          IOC = 86 // ZFS_IOC_VDEV_SET_PROPS
	IOCPoolScrub             IOC = 87 // ZFS_IOC_POOL_SCRUB
	IOCPoolPrefetch          IOC = 88 // ZFS_IOC_POOL_PREFETCH
	IOCDdtPrune              IOC = 89 // ZFS_IOC_DDT_PRUNE
	IOCPoolCondense          IOC = 90 // ZFS_IOC_POOL_CONDENSE

	// Platform block (ZFS_IOC_PLATFORM = 0x80). The EVENTS_* calls are Linux only.
	IOCPlatform    IOC = 0x80
	IOCEventsNext  IOC = 0x81 // ZFS_IOC_EVENTS_NEXT
	IOCEventsClear IOC = 0x82 // ZFS_IOC_EVENTS_CLEAR
	IOCEventsSeek  IOC = 0x83 // ZFS_IOC_EVENTS_SEEK
	IOCNextboot    IOC = 0x84 // ZFS_IOC_NEXTBOOT
	IOCJail        IOC = 0x85 // ZFS_IOC_JAIL
	IOCUnjail      IOC = 0x86 // ZFS_IOC_UNJAIL
	IOCSetBootenv  IOC = 0x87 // ZFS_IOC_SET_BOOTENV
	IOCGetBootenv  IOC = 0x88 // ZFS_IOC_GET_BOOTENV
	iocLast        IOC = 0x89 // ZFS_IOC_LAST
)

var iocNames = map[IOC]string{
	IOCPoolCreate:            "ZFS_IOC_POOL_CREATE",
	IOCPoolDestroy:           "ZFS_IOC_POOL_DESTROY",
	IOCPoolImport:            "ZFS_IOC_POOL_IMPORT",
	IOCPoolExport:            "ZFS_IOC_POOL_EXPORT",
	IOCPoolConfigs:           "ZFS_IOC_POOL_CONFIGS",
	IOCPoolStats:             "ZFS_IOC_POOL_STATS",
	IOCPoolTryimport:         "ZFS_IOC_POOL_TRYIMPORT",
	IOCPoolScan:              "ZFS_IOC_POOL_SCAN",
	IOCPoolFreeze:            "ZFS_IOC_POOL_FREEZE",
	IOCPoolUpgrade:           "ZFS_IOC_POOL_UPGRADE",
	IOCPoolGetHistory:        "ZFS_IOC_POOL_GET_HISTORY",
	IOCVdevAdd:               "ZFS_IOC_VDEV_ADD",
	IOCVdevRemove:            "ZFS_IOC_VDEV_REMOVE",
	IOCVdevSetState:          "ZFS_IOC_VDEV_SET_STATE",
	IOCVdevAttach:            "ZFS_IOC_VDEV_ATTACH",
	IOCVdevDetach:            "ZFS_IOC_VDEV_DETACH",
	IOCVdevSetpath:           "ZFS_IOC_VDEV_SETPATH",
	IOCVdevSetfru:            "ZFS_IOC_VDEV_SETFRU",
	IOCObjsetStats:           "ZFS_IOC_OBJSET_STATS",
	IOCObjsetZplprops:        "ZFS_IOC_OBJSET_ZPLPROPS",
	IOCDatasetListNext:       "ZFS_IOC_DATASET_LIST_NEXT",
	IOCSnapshotListNext:      "ZFS_IOC_SNAPSHOT_LIST_NEXT",
	IOCSetProp:               "ZFS_IOC_SET_PROP",
	IOCCreate:                "ZFS_IOC_CREATE",
	IOCDestroy:               "ZFS_IOC_DESTROY",
	IOCRollback:              "ZFS_IOC_ROLLBACK",
	IOCRename:                "ZFS_IOC_RENAME",
	IOCRecv:                  "ZFS_IOC_RECV",
	IOCSend:                  "ZFS_IOC_SEND",
	IOCInjectFault:           "ZFS_IOC_INJECT_FAULT",
	IOCClearFault:            "ZFS_IOC_CLEAR_FAULT",
	IOCInjectListNext:        "ZFS_IOC_INJECT_LIST_NEXT",
	IOCErrorLog:              "ZFS_IOC_ERROR_LOG",
	IOCClear:                 "ZFS_IOC_CLEAR",
	IOCPromote:               "ZFS_IOC_PROMOTE",
	IOCSnapshot:              "ZFS_IOC_SNAPSHOT",
	IOCDsobjToDsname:         "ZFS_IOC_DSOBJ_TO_DSNAME",
	IOCObjToPath:             "ZFS_IOC_OBJ_TO_PATH",
	IOCPoolSetProps:          "ZFS_IOC_POOL_SET_PROPS",
	IOCPoolGetProps:          "ZFS_IOC_POOL_GET_PROPS",
	IOCSetFsacl:              "ZFS_IOC_SET_FSACL",
	IOCGetFsacl:              "ZFS_IOC_GET_FSACL",
	IOCShare:                 "ZFS_IOC_SHARE",
	IOCInheritProp:           "ZFS_IOC_INHERIT_PROP",
	IOCSmbAcl:                "ZFS_IOC_SMB_ACL",
	IOCUserspaceOne:          "ZFS_IOC_USERSPACE_ONE",
	IOCUserspaceMany:         "ZFS_IOC_USERSPACE_MANY",
	IOCUserspaceUpgrade:      "ZFS_IOC_USERSPACE_UPGRADE",
	IOCHold:                  "ZFS_IOC_HOLD",
	IOCRelease:               "ZFS_IOC_RELEASE",
	IOCGetHolds:              "ZFS_IOC_GET_HOLDS",
	IOCObjsetRecvdProps:      "ZFS_IOC_OBJSET_RECVD_PROPS",
	IOCVdevSplit:             "ZFS_IOC_VDEV_SPLIT",
	IOCNextObj:               "ZFS_IOC_NEXT_OBJ",
	IOCDiff:                  "ZFS_IOC_DIFF",
	IOCTmpSnapshot:           "ZFS_IOC_TMP_SNAPSHOT",
	IOCObjToStats:            "ZFS_IOC_OBJ_TO_STATS",
	IOCSpaceWritten:          "ZFS_IOC_SPACE_WRITTEN",
	IOCSpaceSnaps:            "ZFS_IOC_SPACE_SNAPS",
	IOCDestroySnaps:          "ZFS_IOC_DESTROY_SNAPS",
	IOCPoolReguid:            "ZFS_IOC_POOL_REGUID",
	IOCPoolReopen:            "ZFS_IOC_POOL_REOPEN",
	IOCSendProgress:          "ZFS_IOC_SEND_PROGRESS",
	IOCLogHistory:            "ZFS_IOC_LOG_HISTORY",
	IOCSendNew:               "ZFS_IOC_SEND_NEW",
	IOCSendSpace:             "ZFS_IOC_SEND_SPACE",
	IOCClone:                 "ZFS_IOC_CLONE",
	IOCBookmark:              "ZFS_IOC_BOOKMARK",
	IOCGetBookmarks:          "ZFS_IOC_GET_BOOKMARKS",
	IOCDestroyBookmarks:      "ZFS_IOC_DESTROY_BOOKMARKS",
	IOCRecvNew:               "ZFS_IOC_RECV_NEW",
	IOCPoolSync:              "ZFS_IOC_POOL_SYNC",
	IOCChannelProgram:        "ZFS_IOC_CHANNEL_PROGRAM",
	IOCLoadKey:               "ZFS_IOC_LOAD_KEY",
	IOCUnloadKey:             "ZFS_IOC_UNLOAD_KEY",
	IOCChangeKey:             "ZFS_IOC_CHANGE_KEY",
	IOCRemap:                 "ZFS_IOC_REMAP",
	IOCPoolCheckpoint:        "ZFS_IOC_POOL_CHECKPOINT",
	IOCPoolDiscardCheckpoint: "ZFS_IOC_POOL_DISCARD_CHECKPOINT",
	IOCPoolInitialize:        "ZFS_IOC_POOL_INITIALIZE",
	IOCPoolTrim:              "ZFS_IOC_POOL_TRIM",
	IOCRedact:                "ZFS_IOC_REDACT",
	IOCGetBookmarkProps:      "ZFS_IOC_GET_BOOKMARK_PROPS",
	IOCWait:                  "ZFS_IOC_WAIT",
	IOCWaitFs:                "ZFS_IOC_WAIT_FS",
	IOCVdevGetProps:          "ZFS_IOC_VDEV_GET_PROPS",
	IOCVdevSetProps:          "ZFS_IOC_VDEV_SET_PROPS",
	IOCPoolScrub:             "ZFS_IOC_POOL_SCRUB",
	IOCPoolPrefetch:          "ZFS_IOC_POOL_PREFETCH",
	IOCDdtPrune:              "ZFS_IOC_DDT_PRUNE",
	IOCPoolCondense:          "ZFS_IOC_POOL_CONDENSE",
	IOCPlatform:              "ZFS_IOC_PLATFORM",
	IOCEventsNext:            "ZFS_IOC_EVENTS_NEXT",
	IOCEventsClear:           "ZFS_IOC_EVENTS_CLEAR",
	IOCEventsSeek:            "ZFS_IOC_EVENTS_SEEK",
	IOCNextboot:              "ZFS_IOC_NEXTBOOT",
	IOCJail:                  "ZFS_IOC_JAIL",
	IOCUnjail:                "ZFS_IOC_UNJAIL",
	IOCSetBootenv:            "ZFS_IOC_SET_BOOTENV",
	IOCGetBootenv:            "ZFS_IOC_GET_BOOTENV",
}

// String returns the C name, for example ZFS_IOC_SNAPSHOT.
func (i IOC) String() string {
	if s, ok := iocNames[i]; ok {
		return s
	}
	return "ZFS_IOC_" + itoa(uint32(i))
}

// AllIOCs lists every known ioctl in numeric order.
func AllIOCs() []IOC {
	out := make([]IOC, 0, len(iocNames))
	for i := IOC(0); i < iocLast; i++ {
		if _, ok := iocNames[i]; ok {
			out = append(out, i)
		}
	}
	return out
}
