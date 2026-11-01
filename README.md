# zfs

A pure-Go ZFS client that talks to the kernel through `/dev/zfs` ioctls: no
libzfs, no libzfs_core, no cgo. The sibling of
[go-freebsd/jail](https://github.com/go-freebsd/jail).

> **Status: work in progress.** Reading, properties, creating, snapshots,
> clones, holds, bookmarks, destroy, rename, rollback, promote, encryption keys,
> mounting and send/receive, replication streams (`send -R`/`-p`/`-I`/`-h`) included,
> work and are tested against a real pool on FreeBSD 14.3 and 15.0. Nothing here is
> released yet. Needs Go 1.23.

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
err = snap.Send(ctx, w, zfs.SendIntermediates(first))        // zfs send -I: all snapshots between
err = snap.Send(ctx, w, zfs.Replicate(), zfs.SendHolds())    // zfs send -R -h: the tree, its clones, props, holds
err = snap.Send(ctx, w, zfs.Replicate(), zfs.SendFrom(old))  // zfs send -R -i: incremental
err = snap.Send(ctx, w, zfs.OnProgress(time.Second, func(p zfs.SendProgress) { ... }))

token, ok, _ := dst.ResumeToken()                            // after an interrupted receive -s
err = c.ResumeSend(ctx, w, token)                            // zfs send -t
bm, _ := snap.Redact("red", clonedSnapshot)                  // zfs redact
err = snap.Send(ctx, w, zfs.RedactedWith(bm))                // zfs send --redact
```

| zfs(8) | here |
|---|---|
| `send`, `-i`, `-I` | `Snapshot.Send`, `SendFrom`, `SendIntermediates` |
| `-R` `-p` `-h` `-b` `--skip-missing` | `Replicate`, `SendProps`, `SendHolds`, `SendBackup`, `SkipMissing` |
| `-D` | `SendDedup` (accepted and ignored, as in the CLI) |
| `-L` `-e` `-c` `-w` | `LargeBlocks`, `EmbeddedData`, `Compressed`, `Raw` |
| `-n` (`-v`, `-P`) | `Snapshot.SendSize`, `SendSizes` (one estimate per stream) |
| `-t`, `--saved`, `--redact`, `redact` | `Client.ResumeSend`, `Dataset.SendSaved`, `RedactedWith`, `Snapshot.Redact` |
| `receive`, `-F` `-s` `-c` | `Dataset.Receive`, `Force`, `Resumable`, `Heal` |
| `-o prop=` `-o origin=` | property options, `WithOrigin` |
| `-x prop` | `Exclude` |
| `-u` | `NoMount` |
| `-n` | `DryRun` |
| `-d` `-e`, `-A` | `DiscardPool`, `LastElement`, `Dataset.AbortResume` |
| `-k` | `SkipHolds` |
| `zpool import -o readonly=on` | mounts get `ro`, a send takes no holds, writes fail with `ErrReadOnlyPool` |

`-p`, `-h` and `-b` may start from a bookmark (`From: "#bm"`), as `zfs send -p -i #bm` does; `-R` and `-I` may not.

Single-snapshot streams made here are byte for byte the streams `zfs send` makes, and the CI
feeds them to `zfs receive` and the other way round.

### Replication streams

`zfs send -R`, `-p`, `-I` and `-h` write a header (an XDR nvlist that lists the datasets,
their snapshots, properties, clone origins and holds) in front of one ordinary stream per
snapshot. libzfs makes and takes apart that envelope in userland; here it is done in Go,
so a whole tree goes through one `io.Writer` and comes out of one `io.Reader`:

```go
pr, pw := io.Pipe()
go func() { pw.CloseWithError(top.Send(ctx, pw, zfs.Replicate(), zfs.SendHolds())) }()
got, err := backup.Receive(ctx, pr, zfs.NoMount())   // one Received per snapshot stream
```

The CI compares both directions with the CLI: the header of a stream made here has the
same datasets, snapshots, properties and holds as the one `zfs send` writes, the kernel
streams come in the same order (parents before children, clone origins before clones,
intermediate snapshots only for `-I` and for clone origins) and the stream has the same
length; `zfs receive` takes it and makes the same tree, and `Receive` of the CLI's stream
makes the tree `zfs receive` makes: same snapshot GUIDs, properties as *received* values,
snapshot properties, holds, clones.

`Receive` of an incremental replication stream first brings the target in line with what
the sender did since the base snapshot, as libzfs does (`recv_incremental_replication`):
snapshots and datasets the sender destroyed are destroyed (only with `Force`), renamed
snapshots and datasets are renamed, promoted clones are promoted. What cannot be done is
reported as `ErrPartialReceive` after all data was received. A raw stream of an encrypted
tree gets its encryption roots back afterwards. The tree is mounted once at the end unless
`NoMount` is set.

Differences and limits: `SendSize` of a replication stream adds up the estimates of its
streams and, like `zfs send -n`, counts the first stream of a clone as a full one.
`Intermediates` now writes the replication stream `zfs send -I` writes (earlier versions
wrote the plain streams one after the other; `Receive` takes both). A replication stream
whose stream for a child lacks the child's base snapshot (dropped from the middle) is an
error here. `zfs receive` exits 0 when the sender's pool is the receiving pool: libzfs finds
the base snapshot by GUID on the sender's dataset, resolves the destination to it and ignores
the snapshot that exists there; with a pool of its own libzfs's code fails too (`destination does not
exist`). Receiving into the sender's dataset is not copied. The header is limited
to 128 MiB. Not done: `receive -o` per dataset (the CLI has none either). `ErrReplicationStream` is no longer
returned.

Not possible, because `zfs` refuses them too (`ErrIncompatibleOptions`, before anything is
sent): a resumed, saved or redacted replication stream (`Replicate`/`Intermediates`/`Props`/
`Holds` with `ResumeSend`, `Saved` or `RedactedWith`), and a raw redacted stream. A
replication stream that was cut off is finished the way the CLI does it: receive with
`Resumable()`, resume the stream each leftover token names (`ResumeSend`), then send the
rest as an incremental `-R` (`From`, `Intermediates`) from the newest snapshot every dataset
has; a dataset that did not arrive yet comes in full. (`zfs receive` takes no full stream
onto a dataset that has snapshots, even with `-F`, and neither does `Receive`.)

`Heal` (`zfs receive -c`) makes the checks libzfs makes before the kernel is asked: it cannot be
combined with `Force`, `Resumable`, `NoMount`, `Parents`, `SkipHolds` or a layout
(`ErrIncompatibleOptions`), the target must be an existing snapshot, and its GUID must be the one
of the stream. A FreeBSD test damages the data blocks of a file on a pool of its own, found with
`zdb`, and has `zfs receive -c` and then `Receive` heal it, comparing the file and a scrub; both
refuse the stream of another snapshot.

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

## Destroying datasets

`Dataset.Destroy` is `zfs destroy [-rRfn]`. Without `Recursive()` a dataset with children or
snapshots is refused, with `Recursive()` one whose snapshots have clones elsewhere is, and the
error is a `*DependentsError` that names what is in the way (it matches `ErrExists` or
`ErrBusy`), before anything is changed. `Clones()` (`-R`) destroys those dependents first: a
clone before its origin, the snapshots of a dataset in one batch before the dataset, in the
order libzfs uses. `PlanDestroy` (or `DryRun()`) lists the names in that order without
destroying. The datasets are unmounted first (`ForceUnmount()` for a busy one), the empty mountpoint
directory of a filesystem with a default or inherited mountpoint is removed as libzfs does, a
pool root is refused without `Recursive()` and never destroyed, and under `Recursive()` an
error does not stop the rest (they come back together as a `*BatchError`).

## Destroying snapshots by specification

`Dataset.DestroySnapshots("a%c", zfs.Recursive(), zfs.Deferred())` is `zfs destroy [-rd]
fs@a%c`: a name, a range (`a%c`, `%c`, `a%`, `%`) or a comma separated list, ordered by
creation, all or none. `PlanDestroySnapshots` is the dry run (`zfs destroy -nvp`): the
snapshots and the space they free. As in the CLI, names that do not exist are skipped and an
empty result is `ErrNotFound`; a snapshot with clones is `ErrBusy` (a `*DependentsError`) unless `Deferred()`. With
`Clones()` (`-R`) the clones of the snapshots are destroyed first, at once even if the
snapshots are deferred.

## Diff

`Snapshot.Diff(ctx, other)` and `Snapshot.DiffNow(ctx)` are `zfs diff`: an `iter.Seq2` of
`DiffEntry` (added, removed, modified, renamed, with the file type of `-F`, the change time
of `-t` and the link count change shown as `(+1)`). The kernel reports which objects changed;
the paths and stats come from `OBJ_TO_STATS` as in libzfs, and `DiffNow` makes the temporary
snapshot `zfs diff` makes. Entries are in object order, paths start at the root of the dataset
and are raw. `zfs diff -H` (scripted output) spells every byte that is not a visible ASCII
character, and the backslash, as `\` plus four octal digits (a space is `\0040`); `EscapedPath()`,
`EscapedNewPath()` and `EscapePath` do the same. The CI compares line by line with
`zfs diff -Ft -H` for snapshots, the live filesystem, a clone and names that need escaping.

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
| `DIFF`, `OBJ_TO_STATS`, `NEXT_OBJ`, `TMP_SNAPSHOT` | `Diff` (`zfs diff`) |
| `POOL_CONFIGS` | `Pools` |
| `SET_PROP`, `INHERIT_PROP` | `SetProps`, `SetProp`, `InheritProp` |
| `CREATE`, `CLONE` | `Create`, `CreateVolume`, `Clone` |
| `SNAPSHOT`, `DESTROY_SNAPS`, `DESTROY` | `Snapshot`, `DestroySnapshots`, `DestroySnapshotSpec`, `PlanDestroySnapshots`, `Destroy` |
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
