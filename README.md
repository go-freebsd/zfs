# zfs

A pure-Go ZFS client that talks to the kernel through `/dev/zfs` ioctls: no
libzfs, no libzfs_core, no cgo. The sibling of
[go-freebsd/jail](https://github.com/go-freebsd/jail).

> **Status: work in progress.** Reading, properties, creating, snapshots,
> clones, holds, bookmarks, destroy, rename, rollback, promote and encryption
> keys work and are tested against a real pool on FreeBSD 14.3 and 15.0. Mounting
> and send/receive are next. Nothing here is released yet. Needs Go 1.23.

| Package | What it is |
|---|---|
| `pkg/zfs` | **Start here.** Handles (`Dataset`, `Snapshot`, `Bookmark`), functional options, typed values, `iter.Seq2` listings, sentinel errors. |
| `pkg/primitives` | The low-level client one level below: one method per kernel operation, names and `map[string]any` properties. |
| `pkg/ioctl` | `zfs_cmd_t`, the `ZFS_IOC_*` numbers and `Conn` for `/dev/zfs` (FreeBSD). `Conn.Call` reaches any ioctl. |

The packed-nvlist payload of every ZFS ioctl is handled by
[go-freebsd/nvlist](https://github.com/go-freebsd/nvlist).

## Using it

```go
c, err := zfs.Open()
if err != nil { ... }
defer c.Close()

tank := c.Dataset("tank")                       // a handle; no I/O yet

home, err := tank.CreateFilesystem("home",
    zfs.WithCompression(zfs.ZSTDLevel(5)),
    zfs.WithQuota(10*zfs.GiB),
    zfs.WithMountpoint("/home"),
    zfs.WithProp("acme:owner", "ops"))

secret, err := tank.CreateFilesystem("secret",   // encrypted, key from a passphrase
    zfs.WithEncryption(zfs.AES256GCM, zfs.PassphraseKey("correct horse battery")))

info, err := home.Info()                         // typed state
fmt.Println(info.Used, info.Compression, info.Created, info.Encrypted())

err = home.Set(zfs.WithCompression(zfs.LZ4), zfs.WithAtime(false))
err = home.Inherit("compression")

snap, err := home.Snapshot("daily-1")
snaps, err := home.SnapshotAll(ctx, "nightly")   // the whole tree, atomically
for in, err := range home.Snapshots(ctx) { ... } // oldest first
for in, err := range home.Descendants(ctx, zfs.Depth(2), zfs.OnlyKinds(zfs.KindVolume)) { ... }

clone, err := snap.Clone("tank/clone", zfs.WithCompression(zfs.LZ4))
err = clone.Promote()
err = snap.Hold("backup")
bm, err := snap.Bookmark("bm1")
err = snap.Rollback()
err = home.Destroy(zfs.Recursive())
```

Options say what an operation takes. A property option works for `Create`, `Set` and
`Clone`; `Parents`, `WithEncryption` and `WithVolBlockSize` only for creation;
`Recursive`, `Deferred`, `Depth` and `OnlyKinds` for destroy and listing. An option
given to an operation it does not fit makes the call fail with `ErrInvalidOption`
before anything reaches the kernel. `WithProp(name, value)` sets any property by its
zfs name, including user properties.

Errors are `*OpError` and match `errors.Is` with `ErrNotFound`, `ErrExists`,
`ErrBusy`, `ErrPermission`, `ErrNoSpace` and `ErrKey`; the layer below
(`*primitives.BatchError` with the per-item errno, `*ioctl.Error`) stays reachable
with `errors.As`.

Datasets are created but not mounted: the kernel only creates them. Context is
accepted where a call can take many kernel round trips (listings, `SnapshotAll`);
a single ioctl cannot be cancelled. One `SnapshotAll` call takes one snapshot per
dataset, because that is what the kernel accepts.

## Low level

`pkg/primitives` is what `pkg/zfs` is built on, and is meant for the odd case:

```go
p, _ := primitives.Open()
d, _ := p.Get("tank/home")
d.Props["compression"]    // the raw number the kernel stores
d.Enum("compression")     // "lz4", from the built-in property table
p.SetProps("tank/home", map[string]any{"quota": "10G", "recordsize": "64k"})
```

The property table (`props_table.go`) is checked in CI against the kernel: every
enumerated value is set through `zfs(8)` and the stored number compared, and any
property the kernel returns that the table does not know fails the build.
`quota`, `refquota`, `reservation` and `refreservation` accept `"none"`.
Per-user quotas (`userquota@...`), `refreservation=auto` and `zstd-fast` are not
covered yet.

## Wrapped ioctls

| ioctl | Wrapped as (`primitives.Client`) |
|---|---|
| `OBJSET_STATS` | `Get`, `Exists` |
| `DATASET_LIST_NEXT`, `SNAPSHOT_LIST_NEXT` | `List`, `Walk` |
| `GET_HOLDS`, `HOLD`, `RELEASE` | `Holds`, `Hold`, `Release` |
| `GET_BOOKMARKS`, `BOOKMARK`, `DESTROY_BOOKMARKS` | `Bookmarks`, `CreateBookmark`, `DestroyBookmarks` |
| `SPACE_WRITTEN`, `SPACE_SNAPS` | `SpaceWritten`, `SnapshotRangeSpace` |
| `POOL_CONFIGS` | `Pools` |
| `SET_PROP`, `INHERIT_PROP` | `SetProps`, `SetProp`, `InheritProp` |
| `CREATE`, `CLONE` | `Create`, `CreateVolume`, `Clone` |
| `SNAPSHOT`, `DESTROY_SNAPS`, `DESTROY` | `Snapshot`, `DestroySnapshots`, `Destroy` |
| `RENAME`, `ROLLBACK`, `PROMOTE` | `Rename`, `Rollback`, `Promote` |
| `LOAD_KEY`, `UNLOAD_KEY`, `CHANGE_KEY` | `LoadKey`, `CheckKey`, `UnloadKey`, `ChangeKey`, `InheritKey` |
| everything else | `ioctl.Conn.Call` / `Do` / `Ioctl` |

## Testing

The portable code is unit-tested everywhere: `pkg/primitives` against an in-memory fake
kernel that enforces the list cursor protocol, `pkg/zfs` against a recording backend.
CI also runs FreeBSD 14.3 and 15.0 VMs with a real pool on a file vdev and compares
what this library reads and writes with `zfs(8)` and `zpool(8)` as an independent
oracle: fixtures for reading (`internal/ci/fixtures.sh`), and scenario tests that
create, snapshot, clone, encrypt and destroy, including passphrase keys that must
load in `zfs load-key` and the other way round.
