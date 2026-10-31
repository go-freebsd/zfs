# zfs

A pure-Go ZFS client that talks to the kernel through `/dev/zfs` ioctls: no
libzfs, no libzfs_core, no cgo. The sibling of
[go-freebsd/jail](https://github.com/go-freebsd/jail).

> **Status: work in progress.** Reading, properties, creating, snapshots,
> clones, holds, bookmarks, destroy, rename, rollback, promote, encryption keys,
> mounting and send/receive of single-snapshot streams work and are tested against
> a real pool on FreeBSD 14.3 and 15.0. Replication streams (`send -R`/`-p`) are
> next. Nothing here is released yet. Needs Go 1.23.

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

## Send and receive

Streams go through `io.Writer` and `io.Reader`, so they can be piped to `ssh`, a file,
an object store or `zstd`. An `*os.File` (a file, a pipe, a socket's `File()`) is handed
to the kernel directly; anything else is fed through a pipe.

```go
// zfs send -i -c -L tank/data@new | ssh backup zfs receive -s tank/backup/data
err = snap.Send(ctx, stdin, zfs.SendFrom(old), zfs.Compressed(), zfs.LargeBlocks())

// the other end
got, err := c.Dataset("tank/backup/data").Receive(ctx, os.Stdin, zfs.Resumable(),
    zfs.WithMountpoint("none"), zfs.WithCompression(zfs.ZSTD))

n, err := snap.SendSize(zfs.Compressed())                    // zfs send -n
err = snap.Send(ctx, w, zfs.Raw())                           // zfs send -w: stays encrypted
err = snap.Send(ctx, w, zfs.SendIntermediates(first))        // all snapshots between, in order
err = snap.Send(ctx, w, zfs.OnProgress(time.Second, func(p zfs.SendProgress) { ... }))

token, ok, _ := dst.ResumeToken()                            // after an interrupted receive -s
err = c.ResumeSend(ctx, w, token)                            // zfs send -t
bm, _ := snap.Redact("red", clonedSnapshot)                  // zfs redact
err = snap.Send(ctx, w, zfs.RedactedWith(bm))                // zfs send --redact
```

| zfs(8) | here |
|---|---|
| `send`, `-i`, `-I` | `Snapshot.Send`, `SendFrom`, `SendIntermediates` |
| `-L` `-e` `-c` `-w` | `LargeBlocks`, `EmbeddedData`, `Compressed`, `Raw` |
| `-n` | `Snapshot.SendSize` |
| `-t`, `--saved`, `--redact`, `redact` | `Client.ResumeSend`, `Dataset.SendSaved`, `RedactedWith`, `Snapshot.Redact` |
| `receive`, `-F` `-s` `-c` | `Dataset.Receive`, `Force`, `Resumable`, `Heal` |
| `-o prop=` `-o origin=` | property options, `WithOrigin` |
| `-x prop` | `Exclude` |
| `-u` | `NoMount` |
| `-n` | `DryRun` |
| `-d` `-e`, `-A` | `DiscardPool`, `LastElement`, `Dataset.AbortResume` |

Streams made here are byte for byte the streams `zfs send` makes, and the CI feeds them to
`zfs receive` and the other way round. **Replication streams are not supported**: what
`zfs send -R`, `-p` and `-I` write is a header, a list of filesystems and properties, and
several streams, which libzfs takes apart in userland. `Receive` refuses them with
`ErrReplicationStream` and creates nothing; `SendIntermediates` writes the plain
streams one after the other instead, which `Receive` and `zfs receive` both take.
`Heal` is passed to the kernel but is not tested against a corrupted pool.

`Exclude` (`-x`) works the way libzfs does it: the property is inherited on the target
instead of taking the stream's value. A plain stream carries no properties, so this
changes something only for a target that already holds *received* values, from an earlier
`zfs send -p` received with the CLI. A property set locally on the target, or one that does
not apply to the kind of dataset, is left alone.

`DryRun` (`-n`) reads the stream to its end and checks it the way `zfs receive -n` does
(framing, name, a new filesystem on an existing target needs `Force` and no snapshots, a
missing parent needs `Parents`, raw onto an unencrypted target), and also what the kernel
would refuse: an incremental stream needs its base among the target's snapshots, and the
snapshot must not exist yet. It creates, changes and mounts nothing, and `Received.Bytes`
is the length of the stream. It does not verify data checksums (libzfs does not either) and
does not predict failures only the kernel knows, such as a full pool or a target that was
modified since the base (`ETXTBSY`).

## Mounting

The kernel only creates datasets; mounting is userland work in libzfs, and is done here
the same way: `nmount(2)` with the options the properties imply, the mount table from
`getfsstat(2)`, and libzfs's rules for what is mountable (mountpoint `none`/`legacy`,
`canmount`, `jailed` outside a jail, redacted, encrypted without its key, a non-empty
directory unless `overlay=on`). Like the CLI, the calls that make or move filesystems
mount and unmount them:

```go
home, _ := tank.CreateFilesystem("home", zfs.WithMountpoint("/home")) // mounted
quiet, _ := tank.CreateFilesystem("quiet", zfs.NoMount())             // zfs create -u

where, mounted, _ := home.IsMounted()
err = quiet.Mount()                          // zfs mount; ErrNotMountable, ErrBusy, ErrMountpointNotEmpty
err = quiet.Unmount(zfs.ForceUnmount())      // zfs unmount -f
err = tank.MountAll()                        // zfs mount -a, parents first
err = tank.UnmountAll()                      // children first
home.Set(zfs.WithMountpoint("/srv/home"))    // unmounts it and the children that inherit, mounts them at the new place
home.Rename("tank/srv")                      // same for a rename
err = home.Destroy(zfs.Recursive())          // unmounts first; a busy mount: ErrBusy and nothing is lost
```

`Create`, `Clone` and `Receive` mount what they made (not volumes, not `canmount=noauto|off`,
not an encrypted dataset still waiting for its key); `NoMount` turns that off. `Destroy`, `Rename`
and a change of `mountpoint` or `canmount` unmount the dataset and the children that inherit
the mountpoint first, and mount again what was mounted; if something is busy the ones already
unmounted are mounted again and the call fails, nothing is half done. `ForceUnmount` is `-f`.
`Unmount(AndUnloadKey())` unloads the key of an encryption root. `Mount(WithMountKey(key))`
loads it first. Needs root; without `/dev/zfs` or off FreeBSD the mounting calls fail with
`errors.ErrUnsupported`. The pool's `readonly` property is not looked at.

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
| `SEND_NEW`, `SEND_SPACE`, `SEND_PROGRESS`, `RECV_NEW`, `REDACT` | `Send`, `SendFD`, `SendSize`, `SendProgress`, `Receive`, `ReceiveFD`, `Redact` |
| `LOAD_KEY`, `UNLOAD_KEY`, `CHANGE_KEY` | `LoadKey`, `CheckKey`, `UnloadKey`, `ChangeKey`, `InheritKey` |
| `nmount(2)`, `unmount(2)`, `getfsstat(2)` | `Mount`, `Unmount`, `Mounts`, `IsMounted`, `MountPoint`, `MountAll`, `UnmountAll` |
| everything else | `ioctl.Conn.Call` / `Do` / `Ioctl` |

## Testing

The portable code is unit-tested everywhere: `pkg/primitives` against an in-memory fake
kernel that enforces the list cursor protocol, `pkg/zfs` against a recording backend.
CI also runs FreeBSD 14.3 and 15.0 VMs with a real pool on a file vdev and compares
what this library reads and writes with `zfs(8)` and `zpool(8)` as an independent
oracle: fixtures for reading (`internal/ci/fixtures.sh`), and scenario tests that
create, snapshot, clone, encrypt and destroy, including passphrase keys that must
load in `zfs load-key` and the other way round.
